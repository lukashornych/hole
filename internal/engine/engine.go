// Package engine is the only place in Hole that shells out to the container runtime.
// Keeping every docker/podman invocation here means runtime quirks (podman's different
// inspect shapes, missing prune filters) have exactly one place to be handled.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lukashornych/hole/v2/internal/logging"
)

// Labels Hole puts on every resource it creates. They are the ground truth for garbage
// collection and destroy — more reliable than name prefixes.
const (
	LabelManaged  = "hole.managed"
	LabelInstance = "hole.instance"
	LabelProject  = "hole.project"
)

// Engine is a resolved container runtime.
type Engine struct {
	// Binary is `docker` or `podman` — or whatever $HOLE_RUNTIME named, which may be a path
	// or a wrapper, so flavor decisions go through IsDocker, never the raw name.
	Binary string

	flavorOnce sync.Once
	flavor     string
}

// IsDocker reports whether the runtime is real docker rather than podman. $HOLE_RUNTIME may
// point at a path or a wrapper script, so the binary name alone is not conclusive: the
// `--version` banner is asked once (podman prints "podman version" even through its docker
// alias), with the base name as the fallback when even that fails.
func (e *Engine) IsDocker() bool {
	e.flavorOnce.Do(func() {
		e.flavor = path.Base(e.Binary)
		if out, err := e.output("--version"); err == nil {
			if strings.Contains(strings.ToLower(out), "podman") {
				e.flavor = "podman"
			} else {
				e.flavor = "docker"
			}
		}
	})
	return e.flavor == "docker"
}

// Detect resolves the container runtime: $HOLE_RUNTIME, then docker, then podman. It also
// verifies the compose subcommand works, because everything else depends on it.
func Detect() (*Engine, error) {
	if forced := os.Getenv("HOLE_RUNTIME"); forced != "" {
		if _, err := exec.LookPath(forced); err != nil {
			return nil, fmt.Errorf("HOLE_RUNTIME is set to '%s' but it is not installed or not in PATH", forced)
		}
		return newEngine(forced)
	}
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return newEngine(candidate)
		}
	}
	return nil, fmt.Errorf("neither docker nor podman is installed or in PATH")
}

func newEngine(binary string) (*Engine, error) {
	engine := &Engine{Binary: binary}
	if _, err := engine.output("compose", "version"); err != nil {
		return nil, fmt.Errorf("'%s compose' is not available; please install the compose plugin", binary)
	}
	return engine, nil
}

// run executes a runtime command, forwarding output to the console.
func (e *Engine) run(args ...string) error {
	start := time.Now()
	cmd := exec.Command(e.Binary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
	if err != nil {
		return fmt.Errorf("%s %s: %w", e.Binary, strings.Join(args, " "), err)
	}
	return nil
}

// output executes a runtime command and captures stdout.
func (e *Engine) output(args ...string) (string, error) {
	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(e.Binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
	if err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			e.Binary, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunQuiet executes a command and returns its error without printing to the console. Used
// by best-effort teardown paths that log their own warnings.
func (e *Engine) RunQuiet(args ...string) error {
	_, err := e.output(args...)
	return err
}

// ComposeUp starts services from a generated compose file.
func (e *Engine) ComposeUp(project, file, projectDir string, build bool, services ...string) error {
	args := []string{"compose", "-p", project, "--project-directory", projectDir, "-f", file, "up", "-d"}
	if build {
		args = append(args, "--build")
	}
	args = append(args, services...)
	return e.run(args...)
}

// ComposeDown tears a project down without needing the compose file: compose v2 resolves
// the project from container labels, so teardown never depends on generated files that may
// already be gone.
func (e *Engine) ComposeDown(project string) error {
	return e.RunQuiet("compose", "-p", project, "down", "--remove-orphans")
}

// NetworkOptions describes a network to create.
type NetworkOptions struct {
	Name     string
	Subnet   netip.Prefix
	Internal bool
	Labels   map[string]string
}

// NetworkCreate creates a network with an explicit subnet. Creation is atomic in the
// runtime, so a concurrent start that picked the same subnet fails here rather than
// producing an overlapping network.
func (e *Engine) NetworkCreate(opts NetworkOptions) error {
	args := []string{"network", "create", "--subnet", opts.Subnet.String()}
	if opts.Internal {
		args = append(args, "--internal")
	}
	for _, key := range sortedKeys(opts.Labels) {
		args = append(args, "--label", key+"="+opts.Labels[key])
	}
	args = append(args, opts.Name)
	_, err := e.output(args...)
	return err
}

// NetworkID returns the runtime's full identifier of a network. Docker names the bridge
// interface a network creates after the ID's first 12 characters ("br-<id12>").
func (e *Engine) NetworkID(name string) (string, error) {
	out, err := e.output("network", "inspect", "--format", "{{.Id}}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// hostHelperTimeout bounds a RunHostHelper container: firewall maintenance is a few iptables
// calls, so anything this long is a wedged daemon or a held lock — better a warn-and-continue
// start than one that hangs with no message.
const hostHelperTimeout = 60 * time.Second

// RunHostHelper runs a one-shot container in the daemon's own network namespace with
// NET_ADMIN — for host-side firewall maintenance the unprivileged CLI cannot do itself, in
// the namespace where the daemon's rules actually live (on VM-based engines, the VM's).
// The exit code is the container command's own; -1 when the container could not run at all
// or did not finish within hostHelperTimeout.
func (e *Engine) RunHostHelper(image, entrypoint string, args ...string) (string, int, error) {
	runArgs := []string{"run", "--rm", "--network", "host",
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW",
		"--entrypoint", entrypoint, image}
	runArgs = append(runArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), hostHelperTimeout)
	defer cancel()
	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Binary, runArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(runArgs, " "), time.Since(start).Round(time.Millisecond))
	if ctx.Err() != nil {
		return stdout.String(), -1, fmt.Errorf("%s %s: timed out after %s",
			e.Binary, strings.Join(runArgs, " "), hostHelperTimeout)
	}
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s",
			e.Binary, strings.Join(runArgs, " "), err, strings.TrimSpace(stderr.String()))
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), exitErr.ExitCode(), err
		}
		return stdout.String(), -1, err
	}
	return stdout.String(), 0, nil
}

// NetworkRemove removes a network.
func (e *Engine) NetworkRemove(name string) error {
	return e.RunQuiet("network", "rm", name)
}

// NetworkExists reports whether a network is present.
func (e *Engine) NetworkExists(name string) bool {
	_, err := e.output("network", "inspect", name)
	return err == nil
}

// NetworkInfo is the subset of network metadata Hole needs.
type NetworkInfo struct {
	Name    string
	Subnets []netip.Prefix
	Labels  map[string]string
	// Containers is the number of attached containers.
	Containers int
}

// networkInspectResult accepts both docker's and podman's inspect shapes.
type networkInspectResult struct {
	NameUpper string `json:"Name"`
	NameLower string `json:"name"`
	IPAM      struct {
		Config []struct {
			Subnet string `json:"Subnet"`
		} `json:"Config"`
	} `json:"IPAM"`
	PodmanSubnets []struct {
		Subnet string `json:"subnet"`
	} `json:"subnets"`
	LabelsUpper map[string]string `json:"Labels"`
	LabelsLower map[string]string `json:"labels"`
	Containers  map[string]any    `json:"Containers"`
}

// Networks lists every network with its subnets and labels in one pass.
func (e *Engine) Networks() ([]NetworkInfo, error) {
	ids, err := e.output("network", "ls", "-q")
	if err != nil {
		return nil, err
	}
	var idList []string
	for _, line := range strings.Split(ids, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			idList = append(idList, trimmed)
		}
	}
	if len(idList) == 0 {
		return nil, nil
	}

	raw, err := e.output(append([]string{"network", "inspect"}, idList...)...)
	if err != nil {
		// A network can disappear between ls and inspect; work with whatever came back.
		if strings.TrimSpace(raw) == "" {
			return nil, err
		}
	}
	var results []networkInspectResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return nil, fmt.Errorf("parse network inspect output: %w", err)
	}

	infos := make([]NetworkInfo, 0, len(results))
	for _, result := range results {
		info := NetworkInfo{
			Name:       firstNonEmpty(result.NameUpper, result.NameLower),
			Labels:     mergeLabels(result.LabelsUpper, result.LabelsLower),
			Containers: len(result.Containers),
		}
		for _, config := range result.IPAM.Config {
			if prefix, err := netip.ParsePrefix(config.Subnet); err == nil {
				info.Subnets = append(info.Subnets, prefix)
			}
		}
		for _, config := range result.PodmanSubnets {
			if prefix, err := netip.ParsePrefix(config.Subnet); err == nil {
				info.Subnets = append(info.Subnets, prefix)
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// NetworkNames lists network names matching a name filter.
func (e *Engine) NetworkNames(nameFilter string) []string {
	raw, err := e.output("network", "ls", "--filter", "name="+nameFilter, "--format", "{{.Name}}")
	if err != nil {
		return nil
	}
	return nonEmptyLines(raw)
}

// NetworkConnect attaches a running container to an additional network.
func (e *Engine) NetworkConnect(network, container string) error {
	return e.RunQuiet("network", "connect", network, container)
}

// NetworkDisconnect detaches a container from a network.
func (e *Engine) NetworkDisconnect(network, container string) error {
	return e.RunQuiet("network", "disconnect", network, container)
}

// NetworkAttachedContainers lists the IDs of containers attached to a network.
func (e *Engine) NetworkAttachedContainers(name string) []string {
	raw, err := e.output("network", "inspect", name)
	if err != nil {
		return nil
	}
	var results []networkInspectResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return nil
	}
	var ids []string
	for _, result := range results {
		for id := range result.Containers {
			ids = append(ids, id)
		}
	}
	return ids
}

// VolumeCreate creates a labeled volume.
func (e *Engine) VolumeCreate(name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	for _, key := range sortedKeys(labels) {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, name)
	_, err := e.output(args...)
	return err
}

// VolumeRemove removes a volume.
func (e *Engine) VolumeRemove(name string) error {
	return e.RunQuiet("volume", "rm", name)
}

// VolumeExists reports whether a volume is present.
func (e *Engine) VolumeExists(name string) bool {
	_, err := e.output("volume", "inspect", name)
	return err == nil
}

// VolumesByName lists volume names matching a name filter.
func (e *Engine) VolumesByName(nameFilter string) []string {
	raw, err := e.output("volume", "ls", "-q", "--filter", "name="+nameFilter)
	if err != nil {
		return nil
	}
	return nonEmptyLines(raw)
}

// ImageExists reports whether an image reference is present locally.
func (e *Engine) ImageExists(reference string) bool {
	_, err := e.output("image", "inspect", reference)
	return err == nil
}

// ImageRemove removes an image reference.
func (e *Engine) ImageRemove(reference string) error {
	return e.RunQuiet("rmi", reference)
}

// ImageTag points a second reference at an existing image.
func (e *Engine) ImageTag(source, target string) error {
	return e.RunQuiet("tag", source, target)
}

// ImagesByReference lists image references matching a reference filter.
//
// The runtime's own filter is not trusted on its own: podman answers it at image granularity —
// every name of an image any of whose names matches — so a Hole image that shares an ID with an
// unrelated one drags that one into the result, and callers here feed the result straight to
// ImageRemove. The filter is therefore re-applied to what came back.
func (e *Engine) ImagesByReference(reference string) []string {
	raw, err := e.output("images", "--filter", "reference="+reference, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil
	}
	var matched []string
	for _, listed := range nonEmptyLines(raw) {
		if matchesReference(reference, listed) {
			matched = append(matched, listed)
		}
	}
	return matched
}

// matchesReference reports whether a listed image reference satisfies a `reference=` filter.
// A pattern without a tag matches every tag, as the runtimes' own filters do.
func matchesReference(pattern, listed string) bool {
	patternName, patternTag := splitReference(pattern)
	name, tag := splitReference(listed)
	if patternTag != "" && patternTag != tag {
		return false
	}
	for {
		if ok, err := path.Match(patternName, name); err == nil && ok {
			return true
		}
		// Podman qualifies names with the registry the image came from (localhost/,
		// docker.io/library/) while the patterns are written unqualified. Leading components
		// are dropped one at a time rather than guessing which prefixes a runtime uses.
		_, rest, found := strings.Cut(name, "/")
		if !found {
			return false
		}
		name = rest
	}
}

// splitReference separates a reference into name and tag. The tag is the part after the last
// colon, and only when that colon follows the last slash — otherwise the colon belongs to a
// registry's port.
func splitReference(reference string) (name, tag string) {
	if colon := strings.LastIndex(reference, ":"); colon > strings.LastIndex(reference, "/") {
		return reference[:colon], reference[colon+1:]
	}
	return reference, ""
}

// ImagePruneDangling removes dangling images carrying a label. Dangling-only is the runtime
// default, so tagged images are never touched.
func (e *Engine) ImagePruneDangling(label string) error {
	return e.RunQuiet("image", "prune", "--force", "--filter", "label="+label)
}

// ContainerIDs lists container IDs matching a name filter (running and stopped).
func (e *Engine) ContainerIDs(nameFilter string, includeStopped bool) []string {
	args := []string{"ps", "-q", "--filter", "name=" + nameFilter}
	if includeStopped {
		args = append(args, "-a")
	}
	raw, err := e.output(args...)
	if err != nil {
		return nil
	}
	return nonEmptyLines(raw)
}

// ContainerExists reports whether a container exists (in any state).
func (e *Engine) ContainerExists(name string) bool {
	_, err := e.output("container", "inspect", name)
	return err == nil
}

// ContainerStop stops a container.
func (e *Engine) ContainerStop(name string) error {
	return e.RunQuiet("stop", name)
}

// ContainerRemove force-removes containers.
func (e *Engine) ContainerRemove(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return e.RunQuiet(append([]string{"rm", "-f"}, ids...)...)
}

// ContainerHealthProbeOutput returns the output of the last failed healthcheck probe, or "" if
// the container has no healthcheck or has not recorded a failure. Compose reports only that a
// container is unhealthy, so this is the only way to see what the probe actually said.
func (e *Engine) ContainerHealthProbeOutput(name string) string {
	raw, err := e.output("container", "inspect", "--format",
		"{{range .State.Health.Log}}{{.Output}}{{end}}", name)
	// A container without a healthcheck has no Health key at all: docker fails the template
	// outright, podman renders the zero value. Neither is an error worth reporting.
	if err != nil {
		return ""
	}
	if trimmed := strings.TrimSpace(raw); trimmed != "<no value>" {
		return trimmed
	}
	return ""
}

// ContainerLogs returns a container's combined logs.
func (e *Engine) ContainerLogs(name string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(e.Binary, "logs", name)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// ContainerInfo is the subset of container metadata GC and `hole list` need.
type ContainerInfo struct {
	ID    string
	Name  string
	State string
}

// Running reports whether the container is currently running.
func (c ContainerInfo) Running() bool { return c.State == "running" }

// Containers lists containers (including stopped ones) matching a name filter.
func (e *Engine) Containers(nameFilter string) []ContainerInfo {
	raw, err := e.output("ps", "-a", "--filter", "name="+nameFilter, "--format", "{{.ID}}\t{{.Names}}\t{{.State}}")
	if err != nil {
		return nil
	}
	var infos []ContainerInfo
	for _, line := range nonEmptyLines(raw) {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		infos = append(infos, ContainerInfo{ID: fields[0], Name: fields[1], State: strings.ToLower(fields[2])})
	}
	return infos
}

// ContainerWait blocks until a container exits and returns its exit code.
func (e *Engine) ContainerWait(name string) (int, error) {
	raw, err := e.output("wait", name)
	if err != nil {
		return 0, err
	}
	code, convErr := strconv.Atoi(strings.TrimSpace(raw))
	if convErr != nil {
		return 0, fmt.Errorf("unexpected output from wait: %q", strings.TrimSpace(raw))
	}
	return code, nil
}

// NetworkPrune removes unused networks carrying a label, optionally only those older than
// the given age. The runtime's own filters are used rather than hand-rolled date math.
func (e *Engine) NetworkPrune(label, until string) error {
	args := []string{"network", "prune", "--force", "--filter", "label=" + label}
	if until != "" {
		args = append(args, "--filter", "until="+until)
	}
	return e.RunQuiet(args...)
}

// ContainerExitCode reports a stopped container's exit code.
func (e *Engine) ContainerExitCode(name string) (int, bool) {
	raw, err := e.output("container", "inspect", "-f", "{{.State.ExitCode}}", name)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return code, true
}

// ContainerRestartCount reports how often the runtime has restarted a container under its
// restart policy, and whether the count could be read at all. A rising count is how a
// crash-looping container is told apart from one that simply takes a while to come up.
func (e *Engine) ContainerRestartCount(name string) (int, bool) {
	raw, err := e.output("container", "inspect", "-f", "{{.RestartCount}}", name)
	if err != nil {
		return 0, false
	}
	count, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return count, true
}

// ContainerStarted reports whether a container has left the `created` state: it is running, or
// it ran and is now paused, restarting, exited or dead.
//
// Distinct from ContainerExists for one reason that matters: compose creates a container and
// only starts it once its dependencies are healthy, and `wait` on a never-started container
// returns 0 immediately — indistinguishable from a clean exit. Anything deciding "the agent is
// finished" must look at this, not at existence.
func (e *Engine) ContainerStarted(name string) bool {
	raw, err := e.output("container", "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(raw) != "created"
}

// ContainerRunning reports whether a container is currently running.
func (e *Engine) ContainerRunning(name string) bool {
	raw, err := e.output("container", "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(raw) == "true"
}

// Attach connects the current terminal to a running container. Raw mode, terminal resize
// and Ctrl-C proxying stay the runtime CLI's job — that is the point of shelling out.
//
// Agent containers are TTY-enabled, and a runtime refuses to attach a non-terminal stdin to one
// ("cannot attach stdin to a TTY-enabled container because stdin is not a terminal"). A
// non-interactive run therefore attaches output only: it still streams the agent's output and
// still reports its exit code, instead of failing outright.
func (e *Engine) Attach(name string) error {
	args := []string{"attach", name}
	interactive := IsTerminal(os.Stdin)
	if !interactive {
		args = []string{"attach", "--no-stdin", name}
	}
	cmd := exec.Command(e.Binary, args...)
	if interactive {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mergeLabels(sets ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, set := range sets {
		for key, value := range set {
			out[key] = value
		}
	}
	return out
}

func nonEmptyLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
