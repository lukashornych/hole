package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lukashornych/hole/internal/agents"
	"github.com/lukashornych/hole/internal/compose"
	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/logging"
	"github.com/lukashornych/hole/internal/network"
	"github.com/lukashornych/hole/internal/worktree"
)

// dindImage is the Docker-in-Docker sidecar, pinned by digest: a floating tag would let the
// daemon inside every sandbox change under a fixed Hole version. This digest is the
// `docker:dind-rootless` multi-arch index (linux/amd64 + linux/arm64) as published on
// 2026-08-06. The reference deliberately carries no tag — the digest alone identifies the image,
// and a tag beside it is redundant information that can silently drift out of sync — so the tag
// lives in this comment instead. Refreshing it is a deliberate step, see
// documentation/developer/build-and-release.md.
const dindImage = "docker@sha256:7451e3dc398b11ba2d8183bb7915402683e3d32e5ec8cef835c215f314a65fef"

// dindDataRoot is the rootless daemon's data directory. Unlike a root daemon's
// /var/lib/docker, the rootless entrypoint stores everything under the unprivileged user's
// home, so the instance volume and the stale-lock cleanup below both target this path.
const dindDataRoot = "/home/rootless/.local/share/docker"

// dindEntrypoint wraps the rootless dind entrypoint. It runs as root only long enough to point
// the sidecar's default route at the gateway (route changes need NET_ADMIN, which the
// unprivileged user lacks) and to clear the state a hard-killed daemon leaves in the instance
// volume, then drops to the unprivileged `rootless` user before exec'ing dockerd. Running the
// daemon rootless is what stops a container the agent starts through it from reaching the host,
// even though the sidecar is still privileged (which dind requires). HOME and XDG_RUNTIME_DIR
// must be set explicitly because `su` keeps root's environment, and the rootless daemon needs
// its own. `$@` forwards any dockerd flags (the registry mirror) through to the entrypoint.
const dindEntrypoint = `ip route replace default via ${HOLE_GATEWAY_IP} || echo "WARNING: could not set gateway route" >&2
rm -rf /home/rootless/.local/share/docker/containerd/daemon/io.containerd.metadata.v1.bolt/meta.db-lock
rm -f /run/user/1000/docker.pid
exec su rootless -c "HOME=/home/rootless XDG_RUNTIME_DIR=/run/user/1000 exec dockerd-entrypoint.sh $@"
`

type composeInput struct {
	instanceName   string
	projectName    string
	runTmpDir      string
	buildContext   string
	gatewayConfDir string
	prestartDir    string
	hasPrestart    bool
	sandboxNetwork allocatedNetwork
	internetNet    allocatedNetwork
	gatewayIP      string
	imageRef       string
	gatewayImage   string
	settings       *config.Settings
	host           hostenv.Host
	startupAgent   *agents.Agent
	enabledAgents  []*agents.Agent
	policy         network.Policy
	dockerEnabled  bool
	dindVolume     string
	registryMirror string
	worktreeLinks  []worktree.Link
	// interactive is whether the CLI has a terminal to hand to the agent. Without one the agent
	// service gets no TTY and no open stdin, so its process reads EOF and exits instead of
	// waiting forever for input nobody can send.
	interactive bool
	opts        Options
}

// generateCompose writes the single compose file for this run and returns its path.
func generateCompose(in composeInput) (string, error) {
	// The project mount is added separately: both the agent and the DinD sidecar need it
	// first, while everything the builder collects is shared between them verbatim.
	projectMount := in.projectDir() + ":" + in.projectDir()
	mounts := newMountBuilder(in.host, in.runTmpDir)
	mounts.seen[in.projectDir()] = true
	if err := mounts.addExclusions(in.projectDir(), in.projectDir(), in.settings.Files.Exclude); err != nil {
		return "", err
	}
	if err := mounts.addIncludes(in.settings.Files.Include, in.projectDir()); err != nil {
		return "", err
	}
	libraries, err := mergeLibraries(in.host, in.projectDir(), in.settings.Libraries, in.worktreeLinks, in.opts.Libraries)
	if err != nil {
		return "", err
	}
	if err := mounts.addLibraries(libraries, in.projectDir()); err != nil {
		return "", err
	}
	for _, link := range in.worktreeLinks {
		logging.Debug("git worktree library: %s (read-write=%v)", link.HostPath, link.ReadWrite)
	}

	agentVolumes := append([]string{projectMount}, mounts.mounts...)
	if in.hasPrestart {
		agentVolumes = append(agentVolumes, in.prestartDir+":/tmp/prestart-scripts:ro")
	}

	command, err := agentCommand(in)
	if err != nil {
		return "", err
	}

	labels := resourceLabels(in.instanceName, in.projectName)

	agentEnv := userEnvironment(in.settings.Environment, in.host)
	agentEnv = append(agentEnv,
		"HOLE_GATEWAY_IP="+in.gatewayIP,
		"HOLE_SANDBOX_NETWORK="+in.sandboxNetwork.name,
	)
	if in.dockerEnabled {
		agentEnv = append(agentEnv, "DOCKER_HOST=tcp://docker:2375")
	}

	buildArgs := map[string]string{
		"UID":            in.host.BuildUID(),
		"GID":            in.host.BuildGID(),
		"AGENT_USERNAME": in.host.Username,
		"AGENT_HOME":     in.host.Home,
		"CACHEBUST":      cachebustValue(in.opts.Rebuild),
	}
	if packages := strings.Join(in.settings.Dependencies, " "); packages != "" {
		buildArgs["EXTRA_PACKAGES"] = packages
	}
	if in.settings.Container.BaseImage != "" {
		buildArgs["BASE_IMAGE"] = in.settings.Container.BaseImage
	}

	file := &compose.File{
		Services: map[string]*compose.Service{},
		Networks: map[string]*compose.Network{
			"sandbox":  {External: true, Name: in.sandboxNetwork.name},
			"internet": {External: true, Name: in.internetNet.name},
		},
	}

	file.Services["gateway"] = &compose.Service{
		Image: in.gatewayImage,
		Build: &compose.Build{
			Context:    filepath.Join(in.runTmpDir, "gateway"),
			Dockerfile: "Dockerfile",
		},
		PullPolicy: "never",
		// NET_ADMIN plus IP forwarding is what makes this container a router and firewall.
		CapAdd:  []string{"NET_ADMIN"},
		Sysctls: map[string]string{"net.ipv4.ip_forward": "1"},
		Environment: []string{
			"HOLE_SANDBOX_SUBNET=" + in.sandboxNetwork.subnet.String(),
		},
		Volumes: []string{
			filepath.Join(in.gatewayConfDir, "Corefile") + ":/etc/hole/Corefile:ro",
			filepath.Join(in.gatewayConfDir, "dnsmasq.conf") + ":/etc/hole/dnsmasq.conf:ro",
			filepath.Join(in.gatewayConfDir, "nftables.rules") + ":/etc/hole/nftables.rules:ro",
		},
		ExtraHosts: []string{"host.internal:host-gateway"},
		Networks: map[string]*compose.ServiceNetwork{
			"sandbox":  {IPv4Address: in.gatewayIP},
			"internet": {},
		},
		// on-failure rather than unless-stopped: a crashed gateway should come back, but a
		// daemon restart must not resurrect a sandbox whose CLI is long gone.
		Restart: "on-failure",
		// dig with an explicit type asks exactly one question, unlike `nslookup <name>`, which
		// also asks AAAA and fails if that query is not answered. dig exits 0 on SERVFAIL, so
		// the probe matches the answer itself — and matches the whole line, because dig prints
		// `;; communications error to 127.0.0.1#53` on stdout when nothing is listening.
		Healthcheck: &compose.Healthcheck{
			Test: []string{"CMD-SHELL", fmt.Sprintf(
				"dig +short +time=1 +tries=1 @127.0.0.1 %s A | grep -qx 127.0.0.1", network.HealthZone)},
			Interval: "2s",
			Timeout:  "2s",
			Retries:  10,
		},
		Labels: labels,
	}

	agentService := &compose.Service{
		Image: in.imageRef,
		Build: &compose.Build{
			Context:    in.buildContext,
			Dockerfile: "Dockerfile",
			Args:       buildArgs,
		},
		PullPolicy:  "never",
		StdinOpen:   in.interactive,
		TTY:         in.interactive,
		WorkingDir:  in.projectDir(),
		Command:     command,
		Environment: agentEnv,
		Volumes:     agentVolumes,
		DNS:         []string{in.gatewayIP, "127.0.0.11"},
		CapAdd:      []string{"NET_ADMIN"},
		Networks: map[string]*compose.ServiceNetwork{
			"sandbox": {},
		},
		DependsOn: map[string]compose.Dependency{
			"gateway": compose.ServiceHealthy,
		},
		Labels: labels,
	}
	if limit := in.settings.Container.MemoryLimit; limit != "" {
		agentService.MemLimit = limit
	}
	if limit := in.settings.Container.MemorySwapLimit; limit != "" {
		agentService.MemswapLimit = limit
	}
	if in.dockerEnabled {
		agentService.DependsOn["docker"] = compose.ServiceHealthy
	}
	file.Services["agent"] = agentService

	if in.dockerEnabled {
		dindEnv := []string{"DOCKER_TLS_CERTDIR=", "HOLE_GATEWAY_IP=" + in.gatewayIP}
		dindEnv = append(dindEnv, userEnvironment(in.settings.Environment, in.host)...)

		dindVolumes := []string{projectMount, in.dindVolume + ":" + dindDataRoot}
		// Only the exclusion over-mounts are mirrored, so a container started through the
		// sidecar cannot bind-mount a path the agent was meant not to see. Libraries and
		// includes are deliberately *not* mirrored: the sidecar does not need them and there is
		// no reason to widen what it can see. Build contexts do not need them either — the docker
		// client streams the context, so `docker build` and `buildx` work against paths the
		// daemon cannot see; only a run-time bind mount needs a daemon-side path.
		dindVolumes = append(dindVolumes, mounts.exclusions...)

		dindCommand := []string{}
		if in.registryMirror != "" {
			// Sandbox-internal traffic is not filtered — the gateway only polices egress to the
			// internet — so the daemon can reach the mirror even with default-deny in force.
			dindCommand = append(dindCommand, "--registry-mirror="+in.registryMirror)
		}

		file.Services["docker"] = &compose.Service{
			Image: dindImage,
			// The entrypoint injects the gateway route as root, then drops to the rootless user.
			User:        "root",
			Privileged:  true,
			Entrypoint:  []string{"sh", "-c", dindEntrypoint, "--"},
			Command:     dindCommand,
			Environment: dindEnv,
			Volumes:     dindVolumes,
			DNS:         []string{in.gatewayIP, "127.0.0.11"},
			Networks: map[string]*compose.ServiceNetwork{
				"sandbox": {},
			},
			DependsOn: map[string]compose.Dependency{
				"gateway": compose.ServiceHealthy,
			},
			// The rootless daemon has no /var/run/docker.sock, so the probe must reach it over
			// the TCP endpoint the agent uses rather than the default socket path.
			Healthcheck: &compose.Healthcheck{
				Test:     []string{"CMD", "docker", "-H", "tcp://127.0.0.1:2375", "info"},
				Interval: "3s",
				Timeout:  "5s",
				Retries:  10,
			},
			Labels: labels,
		}
		file.Volumes = map[string]*compose.Volume{
			in.dindVolume: {External: true},
		}
	}

	data, err := compose.Marshal(file)
	if err != nil {
		return "", err
	}
	path := filepath.Join(in.runTmpDir, "docker-compose.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write compose file: %w", err)
	}
	return path, nil
}

func (in composeInput) projectDir() string { return in.opts.ProjectDir }

// agentCommand builds the container command: the agent's own command, then the arguments
// configured for it in settings, then the arguments given after `--`.
//
// The CLI arguments come last so an ad-hoc flag overrides a value flag from settings —
// last one wins. Debug mode replaces the whole command with a shell, and settings arguments
// are simply unused there.
func agentCommand(in composeInput) ([]string, error) {
	if in.opts.Debug {
		return []string{"bash"}, nil
	}
	base, err := in.startupAgent.Command()
	if err != nil {
		return nil, err
	}
	// Only the started agent's arguments apply; entries for other agents are ignored.
	settingsArgs := in.settings.AgentArgs(in.startupAgent.Name)

	command := make([]string, 0, len(base)+len(settingsArgs)+len(in.opts.AgentArgs))
	for _, part := range base {
		command = append(command, expandContainerValue(part, in.host))
	}
	// Configured arguments expand for the same reason `environment` does. Arguments from the
	// command line do not: the user's shell already had its chance at them, so expanding again
	// would either double-substitute or defeat the quoting they used to keep a literal `$`.
	for _, part := range settingsArgs {
		command = append(command, expandContainerValue(part, in.host))
	}
	return append(command, in.opts.AgentArgs...), nil
}

// expandContainerValue resolves references in agent command parts. $HOME must become the
// sandbox home (agents pin nvm-installed node binaries by absolute path); other variables
// come from the host environment.
func expandContainerValue(value string, host hostenv.Host) string {
	replaced := strings.NewReplacer(
		"${HOME}", host.Home,
		"$HOME", host.Home,
	).Replace(value)
	return hostenv.ExpandEnvVars(replaced)
}

// userEnvironment renders the `environment` setting, resolving `$VAR` references against the
// host — 1.x got this from Compose's own interpolation of the generated file, which the `$`
// escaping deliberately removed, so the expansion has to happen here instead.
func userEnvironment(environment map[string]string, host hostenv.Host) []string {
	out := make([]string, 0, len(environment))
	for _, key := range config.SortedKeys(environment) {
		out = append(out, key+"="+expandContainerValue(environment[key], host))
	}
	return out
}
