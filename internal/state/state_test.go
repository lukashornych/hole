package state

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func store(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "instances"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func instance(name string) *Instance {
	return &Instance{
		InstanceName: name,
		InstanceID:   "abc123",
		ProjectPath:  "/projects/demo",
		ProjectName:  "demo-1a2b3c4d",
		Agent:        "claude",
		CLIPID:       os.Getpid(),
		WatchdogPID:  os.Getpid(),
		Networks:     []string{name + "_sandbox", name + "_internet"},
		StartedAt:    time.Now(),
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	s := store(t)
	original := instance("hole-sandbox-demo-1a2b3c4d-abc123")
	original.Settings = json.RawMessage(`{"dependencies":["make"]}`)
	original.SettingsFiles = []string{"/home/dev/.hole/settings.json"}
	original.DinDEnabled = true
	original.DinDVolume = "hole-sandbox-docker-data-x"

	if err := s.Write(original); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := s.Read(original.InstanceName)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if loaded.Agent != original.Agent || loaded.ProjectPath != original.ProjectPath {
		t.Errorf("round trip lost fields: %+v", loaded)
	}
	// The state file is indented for readability, so compare the snapshot by value.
	var snapshot map[string]any
	if err := json.Unmarshal(loaded.Settings, &snapshot); err != nil {
		t.Fatalf("settings snapshot is not valid JSON: %v", err)
	}
	deps, ok := snapshot["dependencies"].([]any)
	if !ok || len(deps) != 1 || deps[0] != "make" {
		t.Errorf("settings snapshot = %s", loaded.Settings)
	}
	if !loaded.DinDEnabled || loaded.DinDVolume == "" {
		t.Errorf("DinD fields lost: %+v", loaded)
	}
	if len(loaded.Networks) != 2 {
		t.Errorf("networks = %v", loaded.Networks)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	s := store(t)
	inst := instance("hole-sandbox-demo-1a2b3c4d-abc123")
	if err := s.Write(inst); err != nil {
		t.Fatal(err)
	}
	inst.Agent = "codex"
	if err := s.Write(inst); err != nil {
		t.Fatal(err)
	}
	// No temp file may survive a successful write, or List would try to parse it.
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly one state file, got %d", len(entries))
	}
	loaded, err := s.Read(inst.InstanceName)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent != "codex" {
		t.Errorf("update did not land: %s", loaded.Agent)
	}
}

func TestExistsAndRemove(t *testing.T) {
	s := store(t)
	inst := instance("hole-sandbox-demo-1a2b3c4d-abc123")
	if s.Exists(inst.InstanceName) {
		t.Error("instance exists before it was written")
	}
	if err := s.Write(inst); err != nil {
		t.Fatal(err)
	}
	if !s.Exists(inst.InstanceName) {
		t.Error("instance does not exist after Write")
	}
	s.Remove(inst.InstanceName)
	if s.Exists(inst.InstanceName) {
		t.Error("instance still exists after Remove")
	}
	// Removing twice must be safe: teardown is idempotent.
	s.Remove(inst.InstanceName)
}

func TestListIsOrderedAndSkipsGarbage(t *testing.T) {
	s := store(t)
	older := instance("hole-sandbox-demo-1a2b3c4d-older0")
	older.StartedAt = time.Now().Add(-time.Hour)
	newer := instance("hole-sandbox-demo-1a2b3c4d-newer0")
	for _, inst := range []*Instance{newer, older} {
		if err := s.Write(inst); err != nil {
			t.Fatal(err)
		}
	}
	// A corrupt file must not break the listing.
	if err := os.WriteFile(filepath.Join(s.Dir(), "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lock files must be ignored.
	if err := os.WriteFile(filepath.Join(s.Dir(), "some.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	instances, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(instances))
	}
	if instances[0].InstanceName != older.InstanceName {
		t.Errorf("listing is not oldest-first: %s came first", instances[0].InstanceName)
	}
}

func TestListMissingDirectoryIsEmpty(t *testing.T) {
	s := &Store{dir: filepath.Join(t.TempDir(), "absent")}
	instances, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("expected no instances, got %d", len(instances))
	}
}

func TestPIDAlive(t *testing.T) {
	if !PIDAlive(os.Getpid()) {
		t.Error("the current process is reported as dead")
	}
	for _, pid := range []int{0, -1} {
		if PIDAlive(pid) {
			t.Errorf("PIDAlive(%d) = true", pid)
		}
	}
	// PID 1 exists on every Unix host and is usually owned by root: the EPERM case must
	// still count as alive, or GC would tear down another user's live sandbox.
	if !PIDAlive(1) {
		t.Error("PID 1 reported as dead — the EPERM case is mishandled")
	}
}

func TestAbandonedRequiresTheCLIGoneAndTheWatchdogDead(t *testing.T) {
	live := os.Getpid()
	// A PID that cannot exist: the kernel would have to hand out a negative one.
	dead := -12345

	cases := []struct {
		name          string
		holdLiveness  bool
		watchdogPID   int
		wantAbandoned bool
	}{
		{"cli running, watchdog alive", true, live, false},
		{"cli running, watchdog dead", true, dead, false},
		{"cli gone, watchdog alive", false, live, false},
		{"cli gone, watchdog dead", false, dead, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := store(t)
			inst := instance("hole-sandbox-demo-1a2b3c4d-abc123")
			inst.WatchdogPID = test.watchdogPID
			if err := s.Write(inst); err != nil {
				t.Fatal(err)
			}
			if test.holdLiveness {
				release, err := s.HoldLiveness(inst.InstanceName)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if got := s.Abandoned(inst); got != test.wantAbandoned {
				t.Errorf("Abandoned() = %v, want %v", got, test.wantAbandoned)
			}
		})
	}
}

func TestCLIGoneTracksTheLivenessLock(t *testing.T) {
	s := store(t)
	name := "hole-sandbox-demo-1a2b3c4d-abc123"

	// No lock file yet: the CLI never got far enough to take one, which counts as gone.
	if !s.CLIGone(name) {
		t.Error("CLIGone should be true before the lock exists")
	}

	release, err := s.HoldLiveness(name)
	if err != nil {
		t.Fatal(err)
	}
	if s.CLIGone(name) {
		t.Error("CLIGone should be false while the lock is held")
	}
	release()
	if !s.CLIGone(name) {
		t.Error("CLIGone should be true once the lock is released")
	}
}

func TestCLIGoneDetectsAnUnreapedProcess(t *testing.T) {
	s := store(t)
	name := "hole-sandbox-demo-1a2b3c4d-zombie"

	// A child that exits but is deliberately not reaped stays a zombie and keeps answering
	// signal 0 — the exact case a PID check gets wrong. The kernel drops its file locks at
	// exit, so the liveness lock reports the truth.
	holder := exec.Command(os.Args[0], "-test.run=TestHelperHoldsLiveness")
	holder.Env = append(os.Environ(), "HOLE_TEST_LIVENESS_DIR="+s.Dir(), "HOLE_TEST_LIVENESS_NAME="+name)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && s.CLIGone(name) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.CLIGone(name) {
		_ = holder.Process.Kill()
		t.Skip("helper never took the lock")
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Deliberately not calling Wait(): the process is now an unreaped zombie.
	if !PIDAlive(holder.Process.Pid) {
		t.Log("the zombie was reaped by something else; the liveness check is still the assertion")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !s.CLIGone(name) {
		time.Sleep(20 * time.Millisecond)
	}
	if !s.CLIGone(name) {
		t.Error("a killed but unreaped CLI is still reported as running")
	}
	_ = holder.Wait()
}

// TestHelperHoldsLiveness is not a test: it is the subprocess used above. It takes the
// liveness lock and then blocks until it is killed.
func TestHelperHoldsLiveness(t *testing.T) {
	dir := os.Getenv("HOLE_TEST_LIVENESS_DIR")
	name := os.Getenv("HOLE_TEST_LIVENESS_NAME")
	if dir == "" || name == "" {
		t.Skip("not running as the liveness helper")
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.HoldLiveness(name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	time.Sleep(2 * time.Minute)
}

func TestRequestTeardownMarker(t *testing.T) {
	s := store(t)
	name := "hole-sandbox-demo-1a2b3c4d-abc123"
	if s.TeardownRequested(name) {
		t.Error("teardown was requested before anyone asked")
	}
	s.RequestTeardown(name)
	if !s.TeardownRequested(name) {
		t.Error("the teardown request was not recorded")
	}
	// Remove clears every sidecar file so a later instance of the same name starts clean.
	s.Remove(name)
	if s.TeardownRequested(name) {
		t.Error("the abort marker survived Remove")
	}
}

func TestUptime(t *testing.T) {
	inst := &Instance{StartedAt: time.Now().Add(-90 * time.Second)}
	if uptime := inst.Uptime(); uptime < 89*time.Second || uptime > 95*time.Second {
		t.Errorf("Uptime() = %s", uptime)
	}
	if (&Instance{}).Uptime() != 0 {
		t.Error("an instance without a start time must report zero uptime")
	}
}

func TestLockPathIsDistinctFromStatePath(t *testing.T) {
	s := store(t)
	if s.Path("x") == s.LockPath("x") {
		t.Error("the lock file must not collide with the state file")
	}
}
