// Package state is the instance registry: one JSON file per running sandbox under
// ~/.hole/instances. Docker labels remain the ground truth for what exists; these files are
// the metadata cache that powers `hole list`, the watchdog's work order, and — crucially —
// let garbage collection tell an abandoned instance from a healthy concurrent one.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// fileSuffix is appended to the instance name to form the state file name.
	fileSuffix = ".json"
	// lockSuffix names the per-instance teardown lock.
	lockSuffix = ".lock"
	// livenessSuffix names the file the CLI holds an exclusive lock on for its whole run.
	livenessSuffix = ".alive"
	// abortSuffix names the marker the CLI drops when startup fails before the agent exists.
	abortSuffix = ".abort"
)

// Flags records the CLI flags a sandbox was started with.
type Flags struct {
	Debug             bool `json:"debug"`
	Rebuild           bool `json:"rebuild"`
	Unrestricted      bool `json:"unrestricted"`
	DumpNetworkAccess bool `json:"dumpNetworkAccess"`
	WithDocker        bool `json:"withDocker"`
}

// Instance is everything teardown, `hole list` and GC need to know about one sandbox. It is
// written before the first Docker resource is created, so an instance is always
// recoverable — even one whose CLI never got as far as starting a container.
type Instance struct {
	InstanceName string `json:"instanceName"`
	InstanceID   string `json:"instanceID"`
	ProjectPath  string `json:"projectPath"`
	ProjectName  string `json:"projectName"`
	Agent        string `json:"agent"`
	Profile      string `json:"profile,omitempty"`
	Flags        Flags  `json:"flags"`

	// SettingsFiles are the settings documents that contributed to this run, for `hole list`.
	SettingsFiles []string `json:"settingsFiles,omitempty"`
	// Settings is the merged settings snapshot. The watchdog runs cleanupHost hooks from it,
	// since it cannot re-read files that may have changed (or vanished) since startup.
	Settings json.RawMessage `json:"settings,omitempty"`

	CLIPID      int      `json:"cliPID"`
	WatchdogPID int      `json:"watchdogPID"`
	Networks    []string `json:"networks,omitempty"`
	Subnets     []string `json:"subnets,omitempty"`
	DinDVolume  string   `json:"dindVolume,omitempty"`
	DinDEnabled bool     `json:"dindEnabled"`
	// RegistryMirror is the pull-through cache URL given to the DinD daemon, empty when the
	// mirror could not be started.
	RegistryMirror string `json:"registryMirror,omitempty"`
	ImageRef       string `json:"imageRef,omitempty"`
	// GatewayImage is this run's gateway image tag. Teardown reuses it for the br_netfilter
	// helper: recomputing it from the tearing binary's own asset hash would name the wrong
	// image after an upgrade.
	GatewayImage string `json:"gatewayImage,omitempty"`
	// BridgeFilterRule records that a DOCKER-USER accept rule was installed for this
	// sandbox's bridge (BridgeFilterBridge), so teardown knows to remove it. Backend and
	// physdev record the exact variant the helper installed, so the manual-removal command
	// teardown may have to print names the rule that actually exists.
	BridgeFilterRule    bool   `json:"bridgeFilterRule,omitempty"`
	BridgeFilterBridge  string `json:"bridgeFilterBridge,omitempty"`
	BridgeFilterBackend string `json:"bridgeFilterBackend,omitempty"`
	BridgeFilterPhysdev bool   `json:"bridgeFilterPhysdev,omitempty"`

	RunTmpDir string    `json:"runTmpDir,omitempty"`
	LogFile   string    `json:"logFile,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	Version   string    `json:"version,omitempty"`
}

// Uptime is how long the sandbox has been running.
func (i *Instance) Uptime() time.Duration {
	if i.StartedAt.IsZero() {
		return 0
	}
	return time.Since(i.StartedAt)
}

// Abandoned reports whether both the CLI and the watchdog are gone. Such an instance can be
// torn down by GC including its *running* containers — a distinction the bash version could
// not make, which is why `kill -9` orphans were unrecoverable there.
//
// The CLI is detected through its liveness lock rather than its PID: a process that has
// exited but not yet been reaped still answers signal 0, so a PID check would call a dead
// CLI alive for as long as its parent shell leaves the zombie around.
func (s *Store) Abandoned(instance *Instance) bool {
	return s.CLIGone(instance.InstanceName) && !PIDAlive(instance.WatchdogPID)
}

// Store is the on-disk instance registry.
type Store struct {
	dir string
}

// NewStore opens (and creates) the registry directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create instance registry %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the registry directory.
func (s *Store) Dir() string { return s.dir }

// Path is the state file path of one instance.
func (s *Store) Path(instanceName string) string {
	return filepath.Join(s.dir, instanceName+fileSuffix)
}

// LockPath is the teardown lock path of one instance.
func (s *Store) LockPath(instanceName string) string {
	return filepath.Join(s.dir, instanceName+lockSuffix)
}

// Write persists an instance, replacing any previous content atomically so a reader never
// sees a half-written file.
func (s *Store) Write(instance *Instance) error {
	data, err := json.MarshalIndent(instance, "", "  ")
	if err != nil {
		return fmt.Errorf("encode instance state: %w", err)
	}
	final := s.Path(instance.InstanceName)
	temp := final + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("write instance state: %w", err)
	}
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("write instance state: %w", err)
	}
	return nil
}

// Read loads one instance by name.
func (s *Store) Read(instanceName string) (*Instance, error) {
	data, err := os.ReadFile(s.Path(instanceName))
	if err != nil {
		return nil, err
	}
	var instance Instance
	if err := json.Unmarshal(data, &instance); err != nil {
		return nil, fmt.Errorf("parse instance state %s: %w", s.Path(instanceName), err)
	}
	return &instance, nil
}

// Remove deletes an instance's state file and lock. Best-effort: a missing file is fine.
func (s *Store) Remove(instanceName string) {
	_ = os.Remove(s.Path(instanceName))
	_ = os.Remove(s.LockPath(instanceName))
	_ = os.Remove(s.LivenessPath(instanceName))
	_ = os.Remove(filepath.Join(s.dir, instanceName+abortSuffix))
}

// Exists reports whether an instance is still registered.
func (s *Store) Exists(instanceName string) bool {
	_, err := os.Stat(s.Path(instanceName))
	return err == nil
}

// List returns every registered instance, oldest first. Unreadable files are skipped rather
// than failing the whole listing.
func (s *Store) List() ([]*Instance, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read instance registry: %w", err)
	}
	var instances []*Instance
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		instance, err := s.Read(strings.TrimSuffix(name, fileSuffix))
		if err != nil {
			continue
		}
		instances = append(instances, instance)
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].StartedAt.Before(instances[j].StartedAt)
	})
	return instances, nil
}

// LivenessPath is the file the CLI holds an exclusive lock on while it runs.
func (s *Store) LivenessPath(instanceName string) string {
	return filepath.Join(s.dir, instanceName+livenessSuffix)
}

// HoldLiveness takes the run's liveness lock and returns a release function.
//
// The lock is the CLI's proof of life: the kernel drops it when the process exits — before
// the parent reaps it — so the watchdog can tell "gone" from "not yet reaped", which a PID
// check cannot.
func (s *Store) HoldLiveness(instanceName string) (func(), error) {
	file, err := os.OpenFile(s.LivenessPath(instanceName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, fmt.Errorf("open liveness lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return func() {}, fmt.Errorf("acquire liveness lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// CLIGone reports whether the CLI that started an instance has exited, by testing whether
// its liveness lock can be taken. A missing lock file means the CLI never got far enough to
// take one, which counts as gone.
func (s *Store) CLIGone(instanceName string) bool {
	file, err := os.OpenFile(s.LivenessPath(instanceName), os.O_RDWR, 0o600)
	if err != nil {
		return true
	}
	defer func() { _ = file.Close() }()

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Still held: the CLI is running.
		return false
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return true
}

// RequestTeardown drops the abort marker, asking the watchdog to tear down now instead of
// waiting for an agent container that will never appear.
//
// A file rather than a signal: a signal sent in the milliseconds before the watchdog installs
// its handler is lost — or worse, kills it.
func (s *Store) RequestTeardown(instanceName string) {
	_ = os.WriteFile(filepath.Join(s.dir, instanceName+abortSuffix), nil, 0o600)
}

// TeardownRequested reports whether the CLI asked for an early teardown.
func (s *Store) TeardownRequested(instanceName string) bool {
	_, err := os.Stat(filepath.Join(s.dir, instanceName+abortSuffix))
	return err == nil
}

// PIDAlive reports whether a process exists, using signal 0. A process that has exited but
// not yet been reaped still answers, so this alone cannot prove a *child* is gone — see
// HoldLiveness.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 performs the permission and existence checks without delivering anything.
	// ESRCH means gone; EPERM means it exists but belongs to someone else.
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
