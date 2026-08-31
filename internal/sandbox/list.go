package sandbox

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/state"
)

// List prints the running sandboxes.
//
// The registry is cross-checked against live containers, so an instance whose resources are
// gone is collected on the spot rather than reported as running.
func List(out io.Writer) error {
	host := hostenv.DetectHost()
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		return err
	}
	containerEngine, err := engine.Detect()
	if err != nil {
		return err
	}

	GC(containerEngine, host, store)

	instances, err := store.List()
	if err != nil {
		return err
	}

	running := runningInstances(store, instances)

	if len(running) == 0 {
		logging.Info("No sandboxes are running.")
		return nil
	}

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "INSTANCE\tAGENT\tPROJECT\tUPTIME\tDOCKER\tNETWORK\tSETTINGS")
	for _, instance := range running {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			instance.InstanceID,
			agentLabel(instance),
			instance.ProjectPath,
			formatUptime(instance.Uptime()),
			yesNo(instance.DinDEnabled),
			firstOr(instance.Networks, "-"),
			settingsLabel(host, instance),
		)
	}
	return writer.Flush()
}

// runningInstances drops the instances GC has already torn down.
//
// The predicate is `Abandoned`, the same one GC and the watchdog use, and not a PID check: an
// exited but unreaped process still answers signal 0, and `PIDAlive` additionally reports true on
// EPERM, so a reused PID owned by another user would list a dead sandbox as running.
func runningInstances(store *state.Store, instances []*state.Instance) []*state.Instance {
	var running []*state.Instance
	for _, instance := range instances {
		if store.Abandoned(instance) {
			continue
		}
		running = append(running, instance)
	}
	return running
}

func agentLabel(instance *state.Instance) string {
	if instance.Profile != "" {
		return instance.Agent + ":" + instance.Profile
	}
	return instance.Agent
}

// settingsLabel names which settings documents were merged for the run.
func settingsLabel(host hostenv.Host, instance *state.Instance) string {
	if len(instance.SettingsFiles) == 0 {
		return "defaults"
	}
	labels := make([]string, 0, len(instance.SettingsFiles))
	for _, file := range instance.SettingsFiles {
		switch {
		case file == host.GlobalSettingsFile():
			labels = append(labels, "global")
		case strings.HasPrefix(file, instance.ProjectPath):
			labels = append(labels, "project")
		default:
			labels = append(labels, filepath.Base(file))
		}
	}
	return strings.Join(labels, "+")
}

func formatUptime(uptime time.Duration) string {
	if uptime <= 0 {
		return "-"
	}
	uptime = uptime.Round(time.Second)
	hours := int(uptime.Hours())
	minutes := int(uptime.Minutes()) % 60
	seconds := int(uptime.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func firstOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}
