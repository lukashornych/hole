package update

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lukashornych/hole/v2/internal/dindregistry"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// UninstallOptions controls what Uninstall removes.
type UninstallOptions struct {
	// RemoveSettings deletes ~/.hole. It holds user data — settings, custom agents, logs — so
	// it is only removed on an explicit yes.
	RemoveSettings bool
	// KeepBinary leaves the executable in place.
	KeepBinary bool
}

// Uninstall removes Hole's Docker resources, optionally its user directory, and the binary.
//
// Every step is best-effort and reported: a user uninstalling wants to know what is left
// behind, not to be stopped halfway by one failure.
func Uninstall(host hostenv.Host, containerEngine *engine.Engine, opts UninstallOptions) {
	logging.Info("Removing Hole's Docker resources...")
	dindregistry.Remove(containerEngine)

	if ids := containerEngine.ContainerIDs("hole-sandbox-", true); len(ids) > 0 {
		if err := containerEngine.ContainerRemove(ids...); err != nil {
			logging.Warn("could not remove some containers: %v", err)
		}
	}
	for _, name := range containerEngine.NetworkNames("hole-sandbox-") {
		if err := containerEngine.NetworkRemove(name); err != nil {
			logging.Warn("could not remove network %s: %v", name, err)
		}
	}
	for _, volume := range containerEngine.VolumesByName("hole-sandbox-") {
		if err := containerEngine.VolumeRemove(volume); err != nil {
			logging.Warn("could not remove volume %s: %v", volume, err)
		}
	}
	for _, image := range containerEngine.ImagesByReference("hole-sandbox/*") {
		if err := containerEngine.ImageRemove(image); err != nil {
			logging.Warn("could not remove image %s: %v", image, err)
		}
	}
	RemoveLegacyInstall(host)

	if opts.RemoveSettings {
		if err := os.RemoveAll(host.HoleDir()); err != nil {
			logging.Warn("could not remove %s: %v", host.HoleDir(), err)
		} else {
			logging.Info("Removed %s", host.HoleDir())
		}
	} else {
		logging.Info("Kept your settings, custom agents and logs in %s", host.HoleDir())
	}

	if opts.KeepBinary {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		logging.Warn("could not locate the hole binary; remove it manually")
		return
	}
	// Unlinking a running executable is fine on Unix: the process keeps its inode until exit.
	if err := os.Remove(executable); err != nil {
		logging.Warn("could not remove %s: %v", executable, err)
		logging.Info("Remove it manually with: rm %s", executable)
		return
	}
	logging.Info("Removed %s", executable)
}

// ConfirmRemoveSettings asks whether the user directory should go too.
//
// Without a terminal the answer is no: an uninstall running from a script must not delete
// settings and custom agents because nobody was there to say otherwise.
func ConfirmRemoveSettings(in io.Reader, out io.Writer, holeDir string, interactive bool) bool {
	if !interactive {
		return false
	}
	fmt.Fprintf(out, "Remove your settings, custom agents and logs in %s? [y/N] ", holeDir)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
