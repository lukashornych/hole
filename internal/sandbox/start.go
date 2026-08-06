package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lukashornych/hole/assets"
	"github.com/lukashornych/hole/internal/agents"
	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/dindregistry"
	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hooks"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/image"
	"github.com/lukashornych/hole/internal/logging"
	"github.com/lukashornych/hole/internal/network"
	"github.com/lukashornych/hole/internal/state"
	"github.com/lukashornych/hole/internal/trust"
	"github.com/lukashornych/hole/internal/update"
	"github.com/lukashornych/hole/internal/version"
	"github.com/lukashornych/hole/internal/worktree"
)

// allocationAttempts bounds subnet allocation retries. Attempt 0 is first-fit; the rest are
// randomised so concurrent starts do not stampede the same candidate.
const allocationAttempts = 10

// Options are the resolved CLI inputs for `hole start`.
type Options struct {
	Agent             string
	Profile           string
	ProjectDir        string
	Debug             bool
	Rebuild           bool
	Unrestricted      bool
	DumpNetworkAccess bool
	WithDocker        bool
	// TrustProject accepts the project settings' host-affecting keys without asking, for
	// runs with no terminal to prompt on.
	TrustProject bool
	// Libraries are raw --library values: PATH[:MOUNT][:rw].
	Libraries []string
	AgentArgs []string
	// LogFile is the run log the watchdog appends to and the CLI relays from.
	LogFile string
}

// Start creates the sandbox, attaches the terminal to the agent and tears everything down
// when the agent exits. It returns the exit code the CLI should use.
func Start(opts Options) (exitCode int, err error) {
	// A signal overrides whatever the interrupted step reported, so the CLI always exits
	// with the conventional code for it. The deferred override runs after the deferred
	// teardown below (defers unwind last-registered-first).
	signalled := &atomic.Int32{}
	defer func() {
		if code := signalled.Load(); code != 0 {
			exitCode = int(code)
		}
	}()

	host := hostenv.DetectHost()
	projectName := hostenv.ProjectName(opts.ProjectDir)
	instanceID, err := hostenv.InstanceID()
	if err != nil {
		return 1, err
	}

	instanceName := hostenv.InstanceName(projectName, instanceID)

	containerEngine, err := engine.Detect()
	if err != nil {
		return 1, err
	}

	runTmpDir, err := host.CreateRunTmpDir()
	if err != nil {
		return 1, err
	}

	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		return 1, err
	}

	instance := &state.Instance{
		InstanceName: instanceName,
		InstanceID:   instanceID,
		ProjectPath:  opts.ProjectDir,
		ProjectName:  projectName,
		Agent:        opts.Agent,
		Profile:      opts.Profile,
		Flags: state.Flags{
			Debug:             opts.Debug,
			Rebuild:           opts.Rebuild,
			Unrestricted:      opts.Unrestricted,
			DumpNetworkAccess: opts.DumpNetworkAccess,
			WithDocker:        opts.WithDocker,
		},
		CLIPID:    os.Getpid(),
		RunTmpDir: runTmpDir,
		LogFile:   opts.LogFile,
		StartedAt: time.Now(),
		Version:   version.Version,
	}

	// Registered before any Docker resource exists, so even a startup that aborts in the
	// next few lines leaves a recoverable instance behind rather than an orphan.
	if err := store.Write(instance); err != nil {
		return 1, err
	}
	// Held for the whole run: it is how the watchdog and GC tell a live CLI from one that has
	// exited but not yet been reaped.
	releaseLiveness, err := store.HoldLiveness(instanceName)
	if err != nil {
		return 1, err
	}
	defer releaseLiveness()

	supervisor := startWatchdog(store, instance)
	defer finishTeardown(containerEngine, host, store, instance, supervisor)
	watchSignals(containerEngine, instance, signalled)

	documents, err := loadSettingsDocument(host, opts.ProjectDir, opts.Profile)
	if err != nil {
		return 1, err
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		return 1, err
	}
	globalSettings, err := config.Decode(documents.globalOnly)
	if err != nil {
		return 1, err
	}

	// Before the settings snapshot below and before any host-side hook: the project file is
	// repository content, and teardown replays cleanupHost from that snapshot, so a start that
	// declines the project's grants must not have recorded one.
	if err := trust.Gate(trust.Options{
		ProjectDir:   opts.ProjectDir,
		SettingsFile: projectSettingsFile(opts.ProjectDir),
		Document:     documents.project,
		Store:        trust.NewStore(host.HoleDir()),
		Interactive:  engine.IsTerminal(os.Stdin),
		PreApproved:  opts.TrustProject,
		In:           os.Stdin,
		Out:          os.Stderr,
	}); err != nil {
		return 1, err
	}

	registry, err := agents.Load(host.UserAgentsDir())
	if err != nil {
		return 1, err
	}
	startupAgent, err := registry.Resolve(opts.Agent)
	if err != nil {
		return 1, err
	}
	enabledAgents, err := registry.ResolveEnabled(settings.Container.EnabledAgents)
	if err != nil {
		return 1, err
	}
	if !containsAgent(enabledAgents, startupAgent.Name) {
		return 1, fmt.Errorf(
			"agent '%s' is not in the enabled agents list (%s); configure it via container.enabledAgents in settings.json",
			startupAgent.Name, strings.Join(agents.EnabledNames(enabledAgents), " "))
	}

	// The snapshot is what runs cleanupHost hooks during teardown — including in the
	// watchdog, which must not depend on settings files that may have changed since.
	if snapshot, err := json.Marshal(documents.merged); err == nil {
		instance.Settings = snapshot
		instance.SettingsFiles = documents.files
		if err := store.Write(instance); err != nil {
			logging.Warn("could not update the instance registry: %v", err)
		}
	}
	hookEnv := hookEnvironment(host, instance)

	policy, err := buildPolicy(settings, enabledAgents, opts.Unrestricted)
	if err != nil {
		return 1, err
	}

	setupScripts := hooks.Resolve(settings.Hooks.Setup, host, opts.ProjectDir, "setup")
	imageIdentity, err := resolveImage(projectName, settings, globalSettings, registry, enabledAgents,
		host, opts.ProjectDir, setupScripts)
	if err != nil {
		return 1, err
	}

	dockerEnabled := settings.Container.Docker || opts.WithDocker
	instance.ImageRef = imageIdentity.Reference()
	gatewayImage := image.GatewayImage(assets.BuildInputsHash())

	if opts.Debug {
		logging.Warn("Debug mode: opening bash shell instead of agent CLI")
		logging.Line()
	}
	logging.Info("Launching sandbox for: %s", opts.ProjectDir)
	logging.Info("Project name: %s", projectName)
	logging.Info("Instance ID: %s", instanceID)
	if opts.Profile != "" {
		logging.Info("Profile: %s", opts.Profile)
	}
	logging.Info("Agent image: %s", imageIdentity.Describe())
	logging.Line()

	if opts.Rebuild {
		removeForRebuild(containerEngine, imageIdentity.Reference(), gatewayImage)
	}

	if err := hooks.RunSetupHost(hooks.Resolve(settings.Hooks.SetupHost, host, opts.ProjectDir, "setupHost"), hookEnv); err != nil {
		return 1, err
	}

	// Runs once per version: clears out resources an older Hole (including the bash 1.x line)
	// left behind before this run creates any of its own.
	update.OnVersionChange(host, containerEngine)

	// Backstop for the one case the watchdog cannot cover — CLI and watchdog killed at once
	// — and the place stale networks, volumes and run directories get reclaimed.
	GC(containerEngine, host, store)

	pool, err := network.ParsePool(settings.Network.SubnetPool)
	if err != nil {
		return 1, err
	}
	logging.Info("Creating sandbox networks...")
	sandboxNet, internetNet, err := createNetworks(containerEngine, pool, instanceName, projectName)
	if err != nil {
		return 1, err
	}
	instance.Networks = []string{sandboxNet.name, internetNet.name}
	instance.Subnets = []string{sandboxNet.subnet.String(), internetNet.subnet.String()}
	if err := store.Write(instance); err != nil {
		logging.Warn("could not update the instance registry: %v", err)
	}

	gatewayIP, err := network.GatewayIP(sandboxNet.subnet)
	if err != nil {
		return 1, err
	}

	if dockerEnabled {
		instance.DinDEnabled = true
		instance.DinDVolume = "hole-sandbox-docker-data-" + instanceName
		// The mirror lives on past this sandbox — that is the point of it — and gets attached
		// to the sandbox network so the DinD daemon can reach it with no internet access.
		if dindregistry.Ensure(containerEngine) && dindregistry.Attach(containerEngine, sandboxNet.name) {
			instance.RegistryMirror = dindregistry.MirrorURL
		}
		if err := containerEngine.VolumeCreate(instance.DinDVolume, resourceLabels(instanceName, projectName)); err != nil {
			return 1, fmt.Errorf("create Docker-in-Docker volume: %w", err)
		}
		if err := store.Write(instance); err != nil {
			logging.Warn("could not update the instance registry: %v", err)
		}
	}

	buildContext, err := materializeBuildContext(runTmpDir, enabledAgents, setupScripts)
	if err != nil {
		return 1, err
	}
	gatewayConfDir, err := writeGatewayArtifacts(runTmpDir, policy)
	if err != nil {
		return 1, err
	}
	prestartDir, hasPrestart, err := materializePrestartScripts(runTmpDir, hooks.Resolve(settings.Hooks.Prestart, host, opts.ProjectDir, "prestart"))
	if err != nil {
		return 1, err
	}

	composeFile, err := generateCompose(composeInput{
		instanceName:   instanceName,
		projectName:    projectName,
		runTmpDir:      runTmpDir,
		buildContext:   buildContext,
		gatewayConfDir: gatewayConfDir,
		prestartDir:    prestartDir,
		hasPrestart:    hasPrestart,
		sandboxNetwork: sandboxNet,
		internetNet:    internetNet,
		gatewayIP:      gatewayIP,
		imageRef:       imageIdentity.Reference(),
		gatewayImage:   gatewayImage,
		settings:       settings,
		host:           host,
		startupAgent:   startupAgent,
		enabledAgents:  enabledAgents,
		policy:         policy,
		dockerEnabled:  dockerEnabled,
		dindVolume:     instance.DinDVolume,
		registryMirror: instance.RegistryMirror,
		worktreeLinks:  worktree.Derive(opts.ProjectDir, worktreeMode(settings)),
		interactive:    engine.IsTerminal(os.Stdin),
		opts:           opts,
	})
	if err != nil {
		return 1, err
	}

	logging.Info("Starting network gateway...")
	if err := containerEngine.ComposeUp(instanceName, composeFile, runTmpDir, opts.Rebuild, "gateway"); err != nil {
		reportServiceStartFailure(containerEngine, instanceName, "gateway")
		return 1, err
	}

	if dockerEnabled {
		logging.Info("Starting Docker-in-Docker sidecar...")
		if err := containerEngine.ComposeUp(instanceName, composeFile, runTmpDir, opts.Rebuild, "docker"); err != nil {
			// Both services wait on the gateway's healthcheck, so a failure here is more often
			// the gateway's than the service's own — report it first, and report both.
			reportServiceStartFailure(containerEngine, instanceName, "gateway")
			reportServiceStartFailure(containerEngine, instanceName, "docker")
			return 1, err
		}
	}

	logging.Info("Starting %s agent...", opts.Agent)
	logging.Line()
	if err := containerEngine.ComposeUp(instanceName, composeFile, runTmpDir, opts.Rebuild, "agent"); err != nil {
		reportServiceStartFailure(containerEngine, instanceName, "gateway")
		reportServiceStartFailure(containerEngine, instanceName, "agent")
		return 1, err
	}

	// Only now that a working image exists: a failed build must not have removed the previous
	// one first.
	collectImages(containerEngine, projectName, imageIdentity, gatewayImage)

	agentContainer := instanceName + "-agent-1"
	logging.Info("Attaching to %s agent...", opts.Agent)
	logging.Line()

	return attach(containerEngine, agentContainer), nil
}

// watchSignals installs the run-wide signal handler. It covers the whole start — including
// the image build, where a Ctrl-C would otherwise kill the process before any resource was
// removed. Stopping the agent container first ends an active attach, so both phases end up
// on the same teardown path.
//
// Once signal.Notify is registered the runtime no longer applies the default action, so a
// second Ctrl-C cannot interrupt teardown half-way. Immunity to a *killed* CLI needs the
// detached watchdog of the next phase.
func watchSignals(containerEngine *engine.Engine, instance *state.Instance, signalled *atomic.Int32) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig, ok := <-signals
		if !ok {
			return
		}
		signalled.Store(int32(exitCodeForSignal(sig)))
		logging.Line()
		logging.Warn("received %s, destroying sandbox...", sig)
		// Stopping the agent container ends an active attach; the interrupted step then
		// returns and the normal deferred teardown runs.
		_ = containerEngine.ContainerStop(instance.InstanceName + "-agent-1")
	}()
}

// exitCodeForSignal returns the conventional 128+signal exit code.
func exitCodeForSignal(sig os.Signal) int {
	switch sig {
	case syscall.SIGINT:
		return 130
	case syscall.SIGTERM:
		return 143
	case syscall.SIGHUP:
		return 129
	default:
		return 1
	}
}

// attach connects the terminal to the agent container and reports the exit code the CLI
// should use.
func attach(containerEngine *engine.Engine, container string) int {
	err := containerEngine.Attach(container)
	if err == nil {
		return 0
	}
	// The container's own status wins over the attach client's, and is checked first. An agent
	// whose command finishes before the attach lands makes the runtime refuse with "cannot
	// attach to a stopped container" and exit 1 — indistinguishable, from the client's exit
	// code alone, from an agent that genuinely exited 1. Where both are available they agree,
	// so consulting the container costs nothing and is right in the case they differ.
	if !containerEngine.ContainerRunning(container) {
		if code, ok := containerEngine.ContainerExitCode(container); ok {
			return code
		}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	logging.Warn("attach to %s failed: %v", container, err)
	return 1
}

// projectSettingsFile is the per-project settings path.
func projectSettingsFile(projectDir string) string {
	return filepath.Join(projectDir, ".hole", "settings.json")
}

// settingsDocuments is the outcome of loading the settings files for one run.
type settingsDocuments struct {
	// merged is the effective document: global, project and the selected profile chain.
	merged config.Document
	// globalOnly is the same pipeline over the global file alone, the baseline the image
	// scope decision compares against.
	globalOnly config.Document
	// project is the project file exactly as read — the input to the trust gate, which must
	// see what the repository asks for and not what the merge produced.
	project config.Document
	// files are the documents that contributed, which `hole list` reports.
	files []string
}

// loadSettingsDocument validates and merges the settings documents.
//
// With a profile selected, its inheritance chain is expanded across both files first, so a
// project profile can extend a globally-defined one and vice versa.
func loadSettingsDocument(host hostenv.Host, projectDir, profile string) (settingsDocuments, error) {
	globalPath := host.GlobalSettingsFile()
	projectPath := projectSettingsFile(projectDir)

	globalDoc, err := config.LoadAndValidate(globalPath, "global settings (~/.hole/settings.json)")
	if err != nil {
		return settingsDocuments{}, err
	}
	projectDoc, err := config.LoadAndValidate(projectPath, "project settings (.hole/settings.json)")
	if err != nil {
		return settingsDocuments{}, err
	}

	documents := settingsDocuments{project: projectDoc}
	if globalDoc != nil {
		documents.files = append(documents.files, globalPath)
	}
	if projectDoc != nil {
		documents.files = append(documents.files, projectPath)
	}
	// An empty chain degenerates to the plain two-way merge, and going through
	// MergeWithProfile keeps the argument-vector handling that plain Merge would dedup away.
	if profile == "" {
		documents.merged = config.MergeWithProfile(globalDoc, projectDoc, nil)
		documents.globalOnly = config.MergeWithProfile(globalDoc, nil, nil)
		return documents, nil
	}

	// A requested profile that no file defines is fatal: running with the base permissions
	// instead of the ones the profile grants would be a silent, wrong sandbox.
	chain, err := config.ResolveChain(globalDoc, projectDoc, profile)
	if err != nil {
		return settingsDocuments{}, err
	}
	logging.Debug("profile chain: %s", strings.Join(chain, " -> "))

	// The global-only baseline keeps the profile applied: a *global* profile is still global,
	// so a profile that only adds runtime settings keeps the shared image.
	documents.merged = config.MergeWithProfile(globalDoc, projectDoc, chain)
	documents.globalOnly = config.MergeWithProfile(globalDoc, nil, chain)
	return documents, nil
}

// buildPolicy folds every allow-list source into the gateway policy: each enabled agent's
// built-in entries plus the user's (1.x) whitelist keys, translated into the host×ports
// model.
func buildPolicy(settings *config.Settings, enabledAgents []*agents.Agent, unrestricted bool) (network.Policy, error) {
	var entries []network.Entry
	for _, agent := range enabledAgents {
		content, err := agent.AllowFile()
		if err != nil {
			return network.Policy{}, err
		}
		parsed, err := network.ParseAllowFile(content, fmt.Sprintf("agent '%s' allow.txt", agent.Name))
		if err != nil {
			return network.Policy{}, err
		}
		entries = append(entries, parsed...)
	}

	for _, raw := range settings.Network.Allow {
		entry, err := network.ParseEntry(raw)
		if err != nil {
			return network.Policy{}, err
		}
		entries = append(entries, entry)
	}

	var hostGateway []network.HostGatewayDomain
	for _, raw := range settings.Network.HostGatewayDomains {
		parsed, err := network.ParseHostGatewayDomain(raw)
		if err != nil {
			return network.Policy{}, err
		}
		if parsed.Domain == "localhost" || parsed.Domain == "127.0.0.1" {
			logging.Warn("hostGatewayDomains entry '%s' resolves locally inside the container and cannot reach the host; use a different domain name", raw)
		}
		hostGateway = append(hostGateway, parsed)
	}

	return network.BuildPolicy(entries, hostGateway, unrestricted), nil
}

// resolveImage decides which image the run uses: the shared one, or a project-specific one.
//
// The global-only configuration is the same document pipeline run over the global file alone
// (with the selected profile still applied — a global profile is still global). Reusing the
// exact code paths the build uses is what keeps the scope decision and the actual build
// context from diverging.
func resolveImage(projectName string, merged, globalOnly *config.Settings, registry *agents.Registry,
	enabledAgents []*agents.Agent, host hostenv.Host, projectDir string, setupScripts []hooks.Script) (image.Identity, error) {

	hostIdentity := image.HostIdentity{
		Username: host.Username,
		Home:     host.Home,
		UID:      host.UID,
		GID:      host.GID,
	}
	buildInputs := assets.BuildInputsHash()

	mergedConfig, err := canonicalConfig(merged, registry, enabledAgents, setupScripts)
	if err != nil {
		return image.Identity{}, err
	}
	userAgentHashes, err := userAgentHashes(enabledAgents)
	if err != nil {
		return image.Identity{}, err
	}

	globalEnabled, err := registry.ResolveEnabled(globalOnly.Container.EnabledAgents)
	if err != nil {
		// A global file naming an agent this machine no longer has must not break the run; the
		// merged configuration is what actually gets built.
		logging.Debug("global-only enabled agents could not be resolved: %v", err)
		globalEnabled = enabledAgents
	}
	globalConfig, err := canonicalConfig(globalOnly, registry, globalEnabled,
		hooks.Resolve(globalOnly.Hooks.Setup, host, projectDir, "setup"))
	if err != nil {
		return image.Identity{}, err
	}

	return image.Resolve(projectName,
		image.Manifest{Config: mergedConfig, Host: hostIdentity, BuildInputs: buildInputs, UserAgents: userAgentHashes},
		image.Manifest{Config: globalConfig, Host: hostIdentity, BuildInputs: buildInputs, UserAgents: userAgentHashes})
}

// canonicalConfig reduces settings to the configuration that determines image content.
func canonicalConfig(settings *config.Settings, _ *agents.Registry, enabledAgents []*agents.Agent,
	setupScripts []hooks.Script) (image.Config, error) {

	var setupShas []string
	for _, script := range setupScripts {
		content, err := script.Content()
		if err != nil {
			return image.Config{}, err
		}
		setupShas = append(setupShas, image.ContentSHA(content))
	}
	return image.Config{
		BaseImage:       settings.Container.BaseImage,
		EnabledAgents:   agents.EnabledNames(enabledAgents),
		Dependencies:    settings.Dependencies,
		SetupScriptShas: setupShas,
	}, nil
}

func userAgentHashes(enabledAgents []*agents.Agent) ([]string, error) {
	var hashes []string
	for _, agent := range enabledAgents {
		if agent.Source != agents.SourceUser {
			continue
		}
		hash, err := hashAgentDir(agent)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, agent.Name+":"+hash)
	}
	return hashes, nil
}

// hashAgentDir digests a user agent's plugin files so editing one invalidates the image.
func hashAgentDir(agent *agents.Agent) (string, error) {
	var parts []string
	err := fs.WalkDir(agent.Dir, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(agent.Dir, path)
		if err != nil {
			return err
		}
		parts = append(parts, path+":"+image.ContentSHA(data))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash user agent '%s': %w", agent.Name, err)
	}
	return image.ContentSHA([]byte(strings.Join(parts, "\n"))), nil
}

type allocatedNetwork struct {
	name   string
	subnet netip.Prefix
}

// createNetworks allocates and creates the two per-instance networks: the internal sandbox
// network the agent lives on, and the bridge network the gateway masquerades out of.
func createNetworks(containerEngine *engine.Engine, pool network.Pool, instanceName, projectName string) (allocatedNetwork, allocatedNetwork, error) {
	sandboxName := instanceName + "_sandbox"
	internetName := instanceName + "_internet"

	// A same-name network can only be a leftover of a crashed run with this instance ID.
	for _, name := range []string{sandboxName, internetName} {
		if containerEngine.NetworkExists(name) {
			logging.Warn("Removing stale network: %s", name)
			_ = containerEngine.NetworkRemove(name)
		}
	}

	var lastErr error
	for attempt := 0; attempt < allocationAttempts; attempt++ {
		infos, err := containerEngine.Networks()
		if err != nil {
			return allocatedNetwork{}, allocatedNetwork{}, fmt.Errorf("list existing networks: %w", err)
		}
		var used []netip.Prefix
		for _, info := range infos {
			used = append(used, info.Subnets...)
		}

		subnets, err := pool.Allocate(used, 2, attempt)
		if err != nil {
			return allocatedNetwork{}, allocatedNetwork{}, err
		}

		sandboxNet := allocatedNetwork{name: sandboxName, subnet: subnets[0]}
		internetNet := allocatedNetwork{name: internetName, subnet: subnets[1]}

		labels := resourceLabels(instanceName, projectName)
		if err := containerEngine.NetworkCreate(engine.NetworkOptions{
			Name: sandboxNet.name, Subnet: sandboxNet.subnet, Internal: true, Labels: labels,
		}); err != nil {
			lastErr = err
			logging.Debug("sandbox network allocation attempt %d failed: %v", attempt, err)
			continue
		}
		if err := containerEngine.NetworkCreate(engine.NetworkOptions{
			Name: internetNet.name, Subnet: internetNet.subnet, Labels: labels,
		}); err != nil {
			lastErr = err
			_ = containerEngine.NetworkRemove(sandboxNet.name)
			logging.Debug("internet network allocation attempt %d failed: %v", attempt, err)
			continue
		}
		logging.Debug("allocated subnets: sandbox=%s internet=%s", sandboxNet.subnet, internetNet.subnet)
		return sandboxNet, internetNet, nil
	}
	return allocatedNetwork{}, allocatedNetwork{}, fmt.Errorf(
		"could not allocate sandbox networks after %d attempts: %w", allocationAttempts, lastErr)
}

func resourceLabels(instanceName, projectName string) map[string]string {
	return map[string]string{
		engine.LabelManaged:  "true",
		engine.LabelInstance: instanceName,
		engine.LabelProject:  projectName,
	}
}

// materializeBuildContext writes the embedded image build context to disk: Dockerfile,
// entrypoint, the enabled agents' install scripts and the resolved setup scripts.
func materializeBuildContext(runTmpDir string, enabledAgents []*agents.Agent, setupScripts []hooks.Script) (string, error) {
	contextDir := filepath.Join(runTmpDir, "agent")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return "", fmt.Errorf("create build context: %w", err)
	}

	agentAssets := assets.Agents()
	for _, name := range []string{"Dockerfile", "entrypoint.sh"} {
		data, err := fs.ReadFile(agentAssets, name)
		if err != nil {
			return "", fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(contextDir, name), data, 0o755); err != nil {
			return "", fmt.Errorf("write %s into build context: %w", name, err)
		}
	}

	for _, agent := range enabledAgents {
		agentDir := filepath.Join(contextDir, "agent-installs", agent.Name)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			return "", fmt.Errorf("create build context for agent '%s': %w", agent.Name, err)
		}
		scripts, err := agent.InstallScripts()
		if err != nil {
			return "", err
		}
		for name, data := range scripts {
			if err := os.WriteFile(filepath.Join(agentDir, name), data, 0o755); err != nil {
				return "", fmt.Errorf("write install script for agent '%s': %w", agent.Name, err)
			}
		}
	}

	setupDir := filepath.Join(contextDir, "setup-scripts")
	if err := os.MkdirAll(setupDir, 0o755); err != nil {
		return "", fmt.Errorf("create setup script directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(setupDir, ".gitkeep"), nil, 0o644); err != nil {
		return "", fmt.Errorf("create setup script placeholder: %w", err)
	}
	for i, script := range setupScripts {
		content, err := script.Content()
		if err != nil {
			return "", err
		}
		name := fmt.Sprintf("%03d-%s", i+1, script.Name())
		if !strings.HasSuffix(name, ".sh") {
			name += ".sh"
		}
		if err := os.WriteFile(filepath.Join(setupDir, name), content, 0o755); err != nil {
			return "", fmt.Errorf("write setup script into build context: %w", err)
		}
	}
	return contextDir, nil
}

// materializePrestartScripts copies prestart hooks into the run directory with numbered
// prefixes; the container entrypoint runs them in that order.
func materializePrestartScripts(runTmpDir string, scripts []hooks.Script) (string, bool, error) {
	dir := filepath.Join(runTmpDir, "prestart-scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("create prestart script directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitkeep"), nil, 0o644); err != nil {
		return "", false, fmt.Errorf("create prestart script placeholder: %w", err)
	}
	for i, script := range scripts {
		content, err := script.Content()
		if err != nil {
			return "", false, err
		}
		name := fmt.Sprintf("%03d-%s", i+1, script.Name())
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o755); err != nil {
			return "", false, fmt.Errorf("write prestart script: %w", err)
		}
	}
	return dir, len(scripts) > 0, nil
}

// writeGatewayArtifacts materializes the gateway build context and its generated
// configuration files.
func writeGatewayArtifacts(runTmpDir string, policy network.Policy) (string, error) {
	buildDir := filepath.Join(runTmpDir, "gateway")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", fmt.Errorf("create gateway build context: %w", err)
	}
	gatewayAssets := assets.Gateway()
	for _, name := range []string{"Dockerfile", "entrypoint.sh"} {
		data, err := fs.ReadFile(gatewayAssets, name)
		if err != nil {
			return "", fmt.Errorf("read embedded gateway %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(buildDir, name), data, 0o755); err != nil {
			return "", fmt.Errorf("write gateway %s: %w", name, err)
		}
	}

	confDir := filepath.Join(runTmpDir, "gateway-conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", fmt.Errorf("create gateway config directory: %w", err)
	}
	artifacts, err := policy.Generate()
	if err != nil {
		return "", err
	}
	files := map[string]string{
		"Corefile":       artifacts.Corefile,
		"dnsmasq.conf":   artifacts.DnsmasqConf,
		"nftables.rules": artifacts.NftablesRule,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(confDir, name), []byte(content), 0o644); err != nil {
			return "", fmt.Errorf("write gateway %s: %w", name, err)
		}
	}
	return confDir, nil
}

func removeForRebuild(containerEngine *engine.Engine, agentImage, gatewayImage string) {
	logging.Info("Removing images before rebuild...")
	for _, reference := range []string{agentImage, gatewayImage} {
		if !containerEngine.ImageExists(reference) {
			continue
		}
		if err := containerEngine.ImageRemove(reference); err != nil {
			logging.Debug("could not remove image %s: %v", reference, err)
		}
	}
}

// worktreeMode resolves the git.worktreeLinks setting; read-only is the default so a sibling
// checkout is readable without being writable.
func worktreeMode(settings *config.Settings) worktree.LinkMode {
	switch settings.Git.WorktreeLinks {
	case string(worktree.LinkOff):
		return worktree.LinkOff
	case string(worktree.LinkReadWrite):
		return worktree.LinkReadWrite
	default:
		return worktree.LinkReadOnly
	}
}

func containsAgent(list []*agents.Agent, name string) bool {
	for _, agent := range list {
		if agent.Name == name {
			return true
		}
	}
	return false
}

// startFailureLogLines is how much of a failed service's output is worth showing: enough for a
// crashing entrypoint, short enough that the compose error stays visible above it.
const startFailureLogLines = 20

// reportServiceStartFailure prints why a service did not come up. Compose reports only
// "dependency failed to start: container ... is unhealthy", which names the symptom and nothing
// else, and teardown removes the container moments later — so this is the one chance to read its
// output. Best-effort throughout: a diagnostic must never mask the error it is explaining.
func reportServiceStartFailure(containerEngine *engine.Engine, instanceName, service string) {
	container := fmt.Sprintf("%s-%s-1", instanceName, service)
	if probe := containerEngine.ContainerHealthProbeOutput(container); probe != "" {
		logging.Warn("%s healthcheck output: %s", service, probe)
	}
	logs, err := containerEngine.ContainerLogs(container)
	if err != nil {
		// The output is the runtime's own error message, not the container's; printing it as
		// container output would misattribute it.
		if !containerEngine.ContainerExists(container) {
			logging.Warn("the %s container no longer exists — it was removed during startup", service)
			return
		}
		logging.Debug("could not read %s logs: %v", container, err)
		return
	}
	logs = strings.TrimSpace(logs)
	if logs == "" {
		return
	}
	lines := strings.Split(logs, "\n")
	if len(lines) > startFailureLogLines {
		lines = lines[len(lines)-startFailureLogLines:]
	}
	logging.Warn("last output from the %s container:", service)
	for _, line := range lines {
		logging.Warn("  %s", line)
	}
}

// cachebustValue is a fresh value on rebuild so every Dockerfile layer after
// `ARG CACHEBUST` re-runs, and a constant otherwise so the cache is used.
func cachebustValue(rebuild bool) string {
	if rebuild {
		return strconv.FormatInt(time.Now().Unix(), 10)
	}
	return "1"
}
