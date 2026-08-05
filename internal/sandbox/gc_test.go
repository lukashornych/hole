package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lukashornych/hole/internal/dindregistry"
	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/state"
)

func TestInstanceOfContainer(t *testing.T) {
	tests := map[string]string{
		"hole-sandbox-demo-1a2b3c4d-abc123-agent-1":   "hole-sandbox-demo-1a2b3c4d-abc123",
		"hole-sandbox-demo-1a2b3c4d-abc123-gateway-1": "hole-sandbox-demo-1a2b3c4d-abc123",
		"hole-sandbox-demo-1a2b3c4d-abc123-docker-1":  "hole-sandbox-demo-1a2b3c4d-abc123",
		// Anything not ours must be ignored rather than mis-parsed into an instance name.
		"unrelated-container": "",
		"my-app-web-1":        "",
		"hole-sandbox-":       "",
		"hole-sandbox-short":  "",
	}
	for name, want := range tests {
		if got := instanceOfContainer(name); got != want {
			t.Errorf("instanceOfContainer(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAnyRunning(t *testing.T) {
	if anyRunning([]engine.ContainerInfo{{State: "exited"}, {State: "created"}}) {
		t.Error("no container is running, but anyRunning said otherwise")
	}
	if !anyRunning([]engine.ContainerInfo{{State: "exited"}, {State: "running"}}) {
		t.Error("a running container was not detected")
	}
}

func TestCollectTmpDirsKeepsLiveAndRecentDirectories(t *testing.T) {
	home := t.TempDir()
	host := hostenv.Host{Home: home}
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(host.TmpRoot(), 0o755); err != nil {
		t.Fatal(err)
	}

	makeDir := func(name string, age time.Duration) string {
		path := filepath.Join(host.TmpRoot(), name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}

	stale := makeDir("run.stale", 48*time.Hour)
	recent := makeDir("run.recent", time.Minute)
	live := makeDir("run.live", 48*time.Hour)
	unrelated := makeDir("something-else", 48*time.Hour)

	// A registered instance protects its run directory however old it is: long sessions are
	// legitimate.
	if err := store.Write(&state.Instance{
		InstanceName: "hole-sandbox-demo-1a2b3c4d-live00",
		RunTmpDir:    live,
		CLIPID:       os.Getpid(),
		WatchdogPID:  os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}

	collectTmpDirs(host, store)

	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale, unowned run directory was not removed")
	}
	for _, keep := range []string{recent, live, unrelated} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("directory %s should have been kept: %v", keep, err)
		}
	}
}

// `hole list` used to test CLIPID with signal 0, which architecture.md rules out: an exited but
// unreaped CLI still answers it, and PIDAlive reports true on EPERM as well — so a torn-down
// sandbox was listed as running. The liveness lock is the predicate that actually holds.
func TestRunningInstancesUsesTheLivenessLockNotThePID(t *testing.T) {
	host := hostenv.Host{Home: t.TempDir()}
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		t.Fatal(err)
	}

	// A live PID that never took the liveness lock: the CLI is gone, whoever owns the PID now.
	dead := &state.Instance{
		InstanceName: "hole-sandbox-demo-1a2b3c4d-dead00",
		CLIPID:       os.Getpid(),
		WatchdogPID:  -1,
	}
	live := &state.Instance{
		InstanceName: "hole-sandbox-demo-1a2b3c4d-live00",
		CLIPID:       os.Getpid(),
		WatchdogPID:  -1,
	}
	for _, instance := range []*state.Instance{dead, live} {
		if err := store.Write(instance); err != nil {
			t.Fatal(err)
		}
	}
	release, err := store.HoldLiveness(live.InstanceName)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	running := runningInstances(store, []*state.Instance{dead, live})
	if len(running) != 1 || running[0].InstanceName != live.InstanceName {
		names := make([]string, 0, len(running))
		for _, instance := range running {
			names = append(names, instance.InstanceName)
		}
		t.Errorf("running = %v, want only %s", names, live.InstanceName)
	}
}

// The image cache is shared by every sandbox, so the network-removal fallback must not take it
// with the instance's own containers.
func TestWithoutRegistryMirrorKeepsSandboxContainers(t *testing.T) {
	attached := []string{
		"hole-sandbox-demo-1a2b3c4d-abc123-agent-1",
		dindregistry.ContainerName,
		"hole-sandbox-demo-1a2b3c4d-abc123-docker-1",
	}
	want := []string{
		"hole-sandbox-demo-1a2b3c4d-abc123-agent-1",
		"hole-sandbox-demo-1a2b3c4d-abc123-docker-1",
	}
	if got := withoutRegistryMirror(attached); !reflect.DeepEqual(got, want) {
		t.Errorf("withoutRegistryMirror = %v, want %v", got, want)
	}
}

func TestFormatUptime(t *testing.T) {
	tests := map[time.Duration]string{
		0:                           "-",
		45 * time.Second:            "45s",
		90 * time.Second:            "1m30s",
		2*time.Hour + 5*time.Minute: "2h05m",
	}
	for uptime, want := range tests {
		if got := formatUptime(uptime); got != want {
			t.Errorf("formatUptime(%s) = %q, want %q", uptime, got, want)
		}
	}
}

func TestSettingsLabel(t *testing.T) {
	host := hostenv.Host{Home: "/home/dev"}
	instance := &state.Instance{ProjectPath: "/projects/demo"}
	if got := settingsLabel(host, instance); got != "defaults" {
		t.Errorf("no settings files should report %q, got %q", "defaults", got)
	}

	instance.SettingsFiles = []string{
		"/home/dev/.hole/settings.json",
		"/projects/demo/.hole/settings.json",
	}
	if got := settingsLabel(host, instance); got != "global+project" {
		t.Errorf("settingsLabel = %q, want global+project", got)
	}
}
