// Package hooks resolves and runs the lifecycle hook scripts: setupHost and cleanupHost on
// the host, prestart inside the container at every start, and setup during the image build.
package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// Script is a hook script resolved to an existing file on the host.
type Script struct {
	Path string
}

// Name is the script's base name, used in log lines and build-context file names.
func (s Script) Name() string { return filepath.Base(s.Path) }

// Content reads the script body.
func (s Script) Content() ([]byte, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read hook script %s: %w", s.Path, err)
	}
	return data, nil
}

// Resolve turns hook entries into existing script paths, in entry order.
//
// An entry containing glob metacharacters is expanded and its matches take its place,
// sorted lexicographically — which is what makes `setup.d/*.sh` with `001-`/`002-` prefixes
// run in a predictable order. A missing script or a pattern that matches nothing is a user
// configuration problem that does not compromise the sandbox, so it is warned about and
// skipped.
func Resolve(entries []config.ScriptEntry, host hostenv.Host, projectDir, kind string) []Script {
	var scripts []Script
	for _, entry := range entries {
		if entry.Script == "" {
			continue
		}
		resolved := host.ResolveHostPath(entry.Script, projectDir)

		if config.HasGlobChars(resolved) {
			matches := expandPathGlob(resolved)
			if len(matches) == 0 {
				logging.Warn("%s hook pattern '%s' matched no scripts, skipping", kind, resolved)
				continue
			}
			for _, match := range matches {
				scripts = append(scripts, Script{Path: match})
			}
			continue
		}

		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			logging.Warn("%s hook script '%s' not found, skipping", kind, resolved)
			continue
		}
		scripts = append(scripts, Script{Path: resolved})
	}
	return scripts
}

// expandPathGlob expands an absolute glob into the regular files it matches, sorted. The
// walk starts at the deepest directory before the first pattern segment, so a pattern never
// scans more of the filesystem than it has to.
func expandPathGlob(pattern string) []string {
	segments := strings.Split(pattern, string(filepath.Separator))
	root := string(filepath.Separator)
	firstPattern := 0
	for i, segment := range segments {
		if config.HasGlobChars(segment) {
			firstPattern = i
			break
		}
	}
	if firstPattern > 0 {
		root = filepath.Join(append([]string{"/"}, segments[:firstPattern]...)...)
	}
	remainder := filepath.Join(segments[firstPattern:]...)

	var matches []string
	for _, relative := range config.ExpandGlob(root, remainder) {
		full := filepath.Join(root, relative)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		matches = append(matches, full)
	}
	sort.Strings(matches)
	return matches
}

// RunSetupHost runs the setupHost scripts in order. A failure aborts startup — the sandbox
// would otherwise run without whatever the hook was supposed to prepare.
func RunSetupHost(scripts []Script, extraEnv []string) error {
	for _, script := range scripts {
		logging.Info("Running setupHost hook: %s...", script.Name())
		if err := runScript(script, extraEnv); err != nil {
			return fmt.Errorf("setupHost hook '%s' failed: %w", script.Name(), err)
		}
	}
	return nil
}

// RunCleanupHost runs the cleanupHost scripts during teardown. Teardown never aborts, so a
// failing script is only warned about.
func RunCleanupHost(scripts []Script, extraEnv []string) {
	for _, script := range scripts {
		logging.Info("Running cleanupHost hook: %s...", script.Name())
		if err := runScript(script, extraEnv); err != nil {
			logging.Warn("cleanupHost hook '%s' exited non-zero; continuing teardown", script.Name())
		}
	}
}

func runScript(script Script, extraEnv []string) error {
	cmd := exec.Command("bash", script.Path)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), extraEnv...)
	return cmd.Run()
}
