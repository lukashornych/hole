package sandbox

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/logging"
	"github.com/lukashornych/hole/internal/state"
)

const (
	// WatchdogCommand is the hidden subcommand the CLI re-executes itself with.
	WatchdogCommand = "__watchdog"

	// relayTimeout bounds the wait for the watchdog's teardown before the CLI takes over.
	relayTimeout = 3 * time.Minute
	// relayPoll is how often the CLI checks the log and the registry while waiting.
	relayPoll = 200 * time.Millisecond
	// watchdogPoll is how often the watchdog looks for the agent container.
	watchdogPoll = 500 * time.Millisecond
	// watchdogComponent tags the watchdog's log records so the CLI can relay exactly those.
	watchdogComponent = "watchdog"
)

// supervisor is the CLI's handle on the detached watchdog process.
type supervisor struct {
	pid int
	// exited is set by the reaping goroutine. The watchdog is this process's child, so it
	// stays a zombie until reaped and would keep answering signal 0 — the PID alone cannot
	// tell whether it is still working.
	exited *atomic.Bool
	// logOffset is where the run log stood when the watchdog was spawned; relaying starts
	// there so the CLI does not echo its own earlier lines.
	logOffset int64
}

// alive reports whether the watchdog is still running.
func (s *supervisor) alive() bool {
	if s == nil || s.pid <= 0 {
		return false
	}
	return !s.exited.Load() && state.PIDAlive(s.pid)
}

// startWatchdog spawns the detached teardown supervisor and records its PID in the registry.
//
// The watchdog — not the CLI — performs teardown in every runtime case. That makes the
// cleanup path single-owner and continuously exercised: the code that runs after `kill -9`
// is the exact code that runs on every clean exit. Because it is setsid'd, closing the
// terminal or pressing Ctrl-C again cannot interrupt teardown half-way.
func startWatchdog(store *state.Store, instance *state.Instance) *supervisor {
	executable, err := os.Executable()
	if err != nil {
		logging.Warn("could not locate the hole binary, teardown will run in-process: %v", err)
		return nil
	}

	sup := &supervisor{exited: &atomic.Bool{}, logOffset: fileSize(instance.LogFile)}

	cmd := exec.Command(executable, WatchdogCommand, instance.InstanceName)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	// Raw subprocess output (compose, docker) joins the run log; the watchdog's own records
	// go there as JSON lines, which is what the CLI relays.
	if logFile, err := os.OpenFile(instance.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer func() { _ = logFile.Close() }()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		logging.Warn("could not start the teardown watchdog, teardown will run in-process: %v", err)
		return nil
	}
	sup.pid = cmd.Process.Pid
	// Reap in the background rather than releasing the process: an unreaped child lingers as
	// a zombie for as long as this process lives, and a zombie still answers signal 0. If the
	// CLI exits first the watchdog is reparented to init, which reaps it there.
	go func() {
		_ = cmd.Wait()
		sup.exited.Store(true)
	}()

	instance.WatchdogPID = sup.pid
	if err := store.Write(instance); err != nil {
		logging.Warn("could not record the watchdog PID: %v", err)
	}
	logging.Debug("teardown watchdog started with PID %d", sup.pid)
	return sup
}

// finishTeardown is the CLI's end of the handoff: wait for the watchdog to finish and mirror
// its progress, or do the work itself if the watchdog is not there.
//
// The user's prompt returns only once the resources are actually gone, so an immediate
// re-start cannot race the previous sandbox's cleanup.
func finishTeardown(containerEngine *engine.Engine, host hostenv.Host, store *state.Store,
	instance *state.Instance, sup *supervisor) {

	if !sup.alive() {
		if sup != nil {
			logging.Debug("watchdog %d is gone, tearing down in-process", sup.pid)
		}
		Teardown(containerEngine, host, store, instance)
		return
	}

	// Ask the watchdog to start now: on an early failure the agent container never appears,
	// and without this the user would wait out the watchdog's polling interval.
	store.RequestTeardown(instance.InstanceName)

	relay := newLogRelay(instance.LogFile, sup.logOffset)
	deadline := time.Now().Add(relayTimeout)
	for time.Now().Before(deadline) {
		relay.pump()
		if !store.Exists(instance.InstanceName) {
			relay.pump()
			return
		}
		if !sup.alive() {
			// The watchdog died mid-teardown; the shared function is idempotent, so finishing
			// its work is safe.
			relay.pump()
			logging.Debug("watchdog %d died before finishing, completing teardown in-process", sup.pid)
			Teardown(containerEngine, host, store, instance)
			return
		}
		time.Sleep(relayPoll)
	}
	logging.Warn("the teardown watchdog did not finish within %s, completing teardown in-process", relayTimeout)
	Teardown(containerEngine, host, store, instance)
}

// RunWatchdog is the hidden `hole __watchdog <instance>` entry point.
func RunWatchdog(instanceName string) int {
	host := hostenv.DetectHost()
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		return 1
	}
	instance, err := store.Read(instanceName)
	if err != nil {
		// Nothing to supervise: the instance was already torn down.
		return 0
	}

	closeLog, err := logging.Setup(logging.Options{LogFile: instance.LogFile, NoColor: true, Quiet: true})
	if err == nil {
		defer closeLog()
	}
	logging.SetComponent(watchdogComponent)

	containerEngine, err := engine.Detect()
	if err != nil {
		return 1
	}

	// Terminal signals must not interrupt teardown. The process is setsid'd, so the terminal
	// cannot reach it anyway; this is belt and braces.
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	agentContainer := instanceName + "-agent-1"
	logging.Debug("watchdog supervising %s", instanceName)

	// Phase 1: until the agent container has *started*, the CLI's liveness is the trigger — a
	// startup that aborts before any container must still have its partial resources removed.
	//
	// Started, not merely created: compose creates the agent container and then blocks on the
	// gateway's healthcheck before starting it, and `wait` on a created container returns 0 at
	// once. Treating that as an exit tears the sandbox down while it is still starting.
	for !containerEngine.ContainerStarted(agentContainer) {
		if store.TeardownRequested(instanceName) {
			logging.Debug("the CLI asked for teardown before the agent container appeared")
			Teardown(containerEngine, host, store, instance)
			return 0
		}
		if store.CLIGone(instanceName) {
			logging.Debug("the CLI exited before the agent container appeared")
			Teardown(containerEngine, host, store, instance)
			return 0
		}
		if !store.Exists(instanceName) {
			return 0
		}
		time.Sleep(watchdogPoll)
	}

	// Phase 2: the agent exists — tear down when it stops, or when the CLI that owns it is
	// gone, whichever happens first.
	//
	// The CLI's liveness has to be polled here, not merely trusted to stop the container. A
	// SIGKILLed CLI runs no cleanup, so the container only stops by side effect: the attach
	// dying hangs up the container's terminal. That works solely for a TTY-enabled container,
	// which is exactly what a non-interactive run does not get — and it never covered an agent
	// that ignores the hangup. The lock is released by the kernel when the CLI dies, so this
	// needs no grace period, and a clean run cannot trip it: the CLI holds the lock until after
	// teardown has finished.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if _, err := containerEngine.ContainerWait(agentContainer); err != nil {
			logging.Debug("waiting for %s ended with: %v", agentContainer, err)
		}
	}()
	for {
		select {
		case <-stopped:
			Teardown(containerEngine, host, store, instance)
			return 0
		case <-time.After(watchdogPoll):
			// Both of phase 1's triggers apply here too: an explicit request, and a CLI that
			// died without being able to make one.
			if store.TeardownRequested(instanceName) {
				logging.Debug("the CLI asked for teardown while %s was still running", agentContainer)
				Teardown(containerEngine, host, store, instance)
				return 0
			}
			if store.CLIGone(instanceName) {
				logging.Debug("the CLI is gone while %s is still running", agentContainer)
				Teardown(containerEngine, host, store, instance)
				return 0
			}
		}
	}
}

// logRelay mirrors the watchdog's log records to this process's console.
type logRelay struct {
	path   string
	offset int64
}

func newLogRelay(path string, offset int64) *logRelay {
	return &logRelay{path: path, offset: offset}
}

// pump prints any watchdog records appended since the last call.
func (r *logRelay) pump() {
	if r.path == "" {
		return
	}
	file, err := os.Open(r.path)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Seek(r.offset, 0); err != nil {
		return
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		r.offset += int64(len(line)) + 1
		var record struct {
			Level     string `json:"level"`
			Message   string `json:"msg"`
			Component string `json:"component"`
		}
		// Non-JSON lines are raw subprocess output; they belong in the log, not the console.
		if err := json.Unmarshal(line, &record); err != nil || record.Component != watchdogComponent {
			continue
		}
		logging.Relay(record.Level, record.Message)
	}
}

func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
