package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lukashornych/hole/v2/assets"
	"github.com/lukashornych/hole/v2/internal/compose"
	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

func validateRootlessSettings(settings *config.Settings, enabled bool, host hostenv.Host) error {
	if !settings.Container.DockerRootless {
		return nil
	}
	if !enabled {
		return fmt.Errorf("container.dockerRootless requires container.docker=true or --with-docker")
	}
	for name, value := range map[string]string{"UID": host.BuildUID(), "GID": host.BuildGID()} {
		id, err := strconv.ParseUint(value, 10, 32)
		if err != nil || id == 0 || id >= 4294901759 {
			return fmt.Errorf("container.dockerRootless requires a non-root user with a valid %s (got %q)", name, value)
		}
	}
	return nil
}

// configureRootless changes only the opt-in sidecar. The outer bridge still belongs
// to the outer engine; the nested daemon's bridges belong to its rootlesskit child.
func configureRootless(in composeInput, file *compose.File) error {
	if err := validateRootlessSettings(in.settings, in.dockerEnabled, in.host); err != nil {
		return err
	}
	contextDir := filepath.Join(in.runTmpDir, "rootless")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return err
	}
	hash := sha256.New()
	err := fs.WalkDir(assets.Rootless, "rootless", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := assets.Rootless.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%s\x00", path, data)
		return os.WriteFile(filepath.Join(contextDir, filepath.Base(path)), data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("materialize rootless image: %w", err)
	}
	uid, gid := in.host.BuildUID(), in.host.BuildGID()
	subID := uint64(100000)
	for _, value := range []string{uid, gid} {
		id, _ := strconv.ParseUint(value, 10, 32)
		if id >= subID {
			subID = id + 1
		}
	}
	fmt.Fprintf(hash, "%s\x00%s\x00%s", dindImage, uid, gid)
	runtimeDir := "/run/user/" + uid
	daemonConfig := map[string]any{
		"hosts":        []string{"unix://" + runtimeDir + "/docker.sock", "tcp://0.0.0.0:2375"},
		"tls":          false,
		"rootless":     true,
		"data-root":    dindDataRoot,
		"exec-root":    runtimeDir + "/exec",
		"pidfile":      runtimeDir + "/docker.pid",
		"live-restore": false,
		// vfs needs neither a storage device (/dev/fuse) nor userns overlay support.
		"storage-driver":   "vfs",
		"features":         map[string]bool{"containerd-snapshotter": false},
		"dns":              []string{in.gatewayIP},
		"mtu":              1500,
		"host-gateway-ips": []string{"HOLE_HOST_GATEWAY_IP"},
	}
	// Keep Docker's default bridge: unlike the cohost reference, this is a nested
	// sidecar API whose callers expect plain `docker run` to have a default network.
	if in.registryMirror != "" {
		daemonConfig["registry-mirrors"] = []string{in.registryMirror}
	}
	configJSON, err := json.MarshalIndent(daemonConfig, "", "  ")
	if err != nil {
		return err
	}
	configPath := filepath.Join(in.runTmpDir, "rootless-daemon.json")
	if err := os.WriteFile(configPath, configJSON, 0o644); err != nil {
		return err
	}
	sidecar := file.Services["docker"]
	sidecar.Image = "hole-sandbox/rootless:" + hex.EncodeToString(hash.Sum(nil))[:16]
	sidecar.Build = &compose.Build{Context: contextDir, Dockerfile: "Dockerfile", Args: map[string]string{
		"DIND_IMAGE": dindImage, "UID": uid, "GID": gid, "SUBID_START": strconv.FormatUint(subID, 10),
	}}
	sidecar.PullPolicy = "never"
	sidecar.User = uid + ":" + gid
	sidecar.Privileged = false
	// Allow namespace/mount syscalls without granting SYS_ADMIN or NET_ADMIN. SETUID
	// and SETGID in the bounding set are needed by the setuid mapping helpers.
	// TUN is needed to create slirp's TAP interface in the child network namespace.
	// Device access does not grant NET_ADMIN in the outer namespace. No mknod
	// permission is needed: the runtime supplies the existing character device.
	sidecar.Devices = []string{"/dev/net/tun:/dev/net/tun:rw"}
	sidecar.CapDrop = []string{"ALL"}
	sidecar.CapAdd = []string{"SETUID", "SETGID"}
	sidecar.SecurityOpt = []string{"seccomp=unconfined", "apparmor=unconfined", "systempaths=unconfined"}
	sidecar.Entrypoint = []string{"/usr/local/bin/launch.sh"}
	sidecar.Command = nil
	// Arbitrary project environment must not select a different daemon/helper or
	// override the pinned configuration. Agent and hook environments are unchanged.
	sidecar.Environment = []string{"HOME=/home/rootless", "XDG_RUNTIME_DIR=" + runtimeDir, "HOLE_GATEWAY_IP=" + in.gatewayIP}
	if in.settings.Container.MemoryLimit != "" || in.settings.Container.MemorySwapLimit != "" {
		sidecar.Environment = append(sidecar.Environment, "HOLE_REQUIRE_CGROUP=1")
	}
	sidecar.DNS = []string{in.gatewayIP}
	sidecar.ExtraHosts = []string{"host.internal:host-gateway"}
	sidecar.Volumes = append(sidecar.Volumes, compose.Binds(configPath+":/etc/hole/daemon.json:ro")...)
	sidecar.Healthcheck = &compose.Healthcheck{Test: []string{"CMD", "/usr/local/bin/health.sh"}, Interval: "3s", Timeout: "5s", Retries: 30}
	// Only this short-lived helper receives NET_ADMIN in the OUTER sidecar netns.
	// No host networking, mounts, PID sharing, or host-level nsenter is required.
	file.Services["docker-route"] = &compose.Service{
		Image: in.gatewayImage, User: "0", NetworkMode: "service:docker",
		CapDrop: []string{"ALL"}, CapAdd: []string{"NET_ADMIN"},
		Entrypoint: []string{"ip"}, Command: []string{"route", "replace", "default", "via", in.gatewayIP},
		DependsOn: map[string]compose.Dependency{"docker": {Condition: "service_started"}},
		Labels:    resourceLabels(in.instanceName, in.projectName),
	}
	file.Services["agent"].DependsOn["docker-route"] = compose.Dependency{Condition: "service_completed_successfully"}
	file.Services["agent"].Environment = append(file.Services["agent"].Environment,
		"HOLE_DOCKER_ROOTLESS=1", "HOLE_DOCKER_CONTAINER="+in.instanceName+"-docker-1")
	return nil
}

// Verify startup before starting the agent (and therefore before prestart hooks).
// A failed prerequisite must surface the sidecar logs, not an unrelated agent error.
func waitRootlessReady(e *engine.Engine, instance string, interrupted func() bool) error {
	container := instance + "-docker-1"
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if interrupted() {
			return errInterrupted
		}
		if !e.ContainerRunning(container) {
			return fmt.Errorf("rootless Docker sidecar exited; see prerequisite error in its startup logs")
		}
		switch e.ContainerHealth(container) {
		case "healthy":
			ns, err := e.Exec(container, "hole-dockerd-netns")
			if err != nil {
				return fmt.Errorf("resolve rootless dockerd network namespace: %w", err)
			}
			logging.Debug("Rootless dockerd namespace (sidecar-local PID): %s", ns)
			return nil
		case "unhealthy":
			return fmt.Errorf("rootless Docker sidecar is unhealthy: %s", e.ContainerHealthProbeOutput(container))
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("rootless Docker sidecar did not become ready within 120s: %s", e.ContainerHealthProbeOutput(container))
}
