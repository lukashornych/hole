package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

const settingsUsage = "Usage: hole settings {create|open|get} {project {path}|global} [--overwrite|--append]"

// settingsIO is the process I/O the settings subcommands use, injected for tests.
type settingsIO struct {
	// stdin is one reader for the whole run, shared by every reopen prompt. Re-wrapping
	// os.Stdin per prompt could swallow input buffered between loop iterations.
	stdin *bufio.Reader
	// stdout receives the path printed by `get` and nothing else.
	stdout io.Writer
	// stderr receives the reopen prompt.
	stderr io.Writer
	// terminal is true when stdin and stdout are both terminals.
	terminal bool
}

// settingsTarget is the settings file a `hole settings` subcommand acts on.
type settingsTarget struct {
	// path is the absolute file path, the same one `hole start` reads.
	path string
	// label names the file in validation errors, worded exactly as `hole start` words it.
	label string
	// overrideVar is the environment variable that redirected path, or "" for the default.
	overrideVar string
}

func runSettings(inv *Invocation, sio settingsIO) int {
	if len(inv.Positional) == 0 {
		logging.Error("no settings subcommand given")
		logging.Info(settingsUsage)
		return 1
	}
	subcommand := inv.Positional[0]
	switch subcommand {
	case "create", "open", "get":
	default:
		logging.Error("invalid settings subcommand '%s'; expected create, open or get", subcommand)
		return 1
	}
	if (inv.Overwrite || inv.Append) && subcommand != "create" {
		logging.Error("--overwrite and --append can only be used with settings create")
		return 1
	}
	if inv.Overwrite && inv.Append {
		logging.Error("--overwrite and --append cannot be used together")
		return 1
	}
	if len(inv.Positional) == 1 {
		logging.Error("no settings scope given; expected project or global")
		return 1
	}
	scope := inv.Positional[1]
	var projectArg string
	switch scope {
	case "global":
		if len(inv.Positional) > 2 {
			logging.Error("unexpected argument '%s'; %s", inv.Positional[2], settingsUsage)
			return 1
		}
	case "project":
		if len(inv.Positional) == 2 {
			logging.Error("no project path given")
			logging.Info("%s — use '.' for the current directory", settingsUsage)
			return 1
		}
		if len(inv.Positional) > 3 {
			logging.Error("unexpected argument '%s'; %s", inv.Positional[3], settingsUsage)
			return 1
		}
		projectArg = inv.Positional[2]
	default:
		logging.Error("invalid settings scope '%s'; expected project or global", scope)
		return 1
	}

	target, err := resolveSettingsTarget(hostenv.DetectHost(), scope, projectArg)
	if err != nil {
		logging.Error("%v", err)
		return 1
	}
	if target.overrideVar != "" {
		logging.Info("using %s from %s", target.path, target.overrideVar)
	}
	createHint := "hole settings create global"
	if scope == "project" {
		createHint = "hole settings create project " + projectArg
	}
	switch subcommand {
	case "create":
		return settingsCreate(inv, target)
	case "open":
		return settingsOpen(sio, target, createHint)
	default:
		return settingsGet(sio, target, createHint)
	}
}

// resolveSettingsTarget picks the settings file for a scope, honouring the HOLE_SETTINGS_FILE
// and HOLE_PROJECT_SETTINGS_FILE overrides the way `hole start` does. Unlike
// hostenv.Host.ValidateSettingsFile it does not require an override to exist, because
// `settings create` must be able to create it.
func resolveSettingsTarget(host hostenv.Host, scope, projectArg string) (settingsTarget, error) {
	switch scope {
	case "global":
		override := host.GlobalSettingsOverride
		if override != "" && !filepath.IsAbs(override) {
			return settingsTarget{}, fmt.Errorf("HOLE_SETTINGS_FILE '%s' must be an absolute path", override)
		}
		path := host.GlobalSettingsFile()
		target := settingsTarget{path: path, label: fmt.Sprintf("global settings (%s)", path)}
		if override != "" {
			target.overrideVar = "HOLE_SETTINGS_FILE"
		}
		return target, nil
	default: // "project"; runSettings has already rejected any other scope
		// Validated even when an override redirects the file, exactly as `hole start` does.
		projectDir, err := hostenv.ResolveProjectDir(projectArg)
		if err != nil {
			return settingsTarget{}, err
		}
		override := host.ProjectSettingsOverride
		if override != "" && !filepath.IsAbs(override) {
			return settingsTarget{}, fmt.Errorf("HOLE_PROJECT_SETTINGS_FILE '%s' must be an absolute path", override)
		}
		path := host.ProjectSettingsFile(projectDir)
		target := settingsTarget{path: path, label: "project settings (.hole/settings.json)"}
		if override != "" {
			target.label = fmt.Sprintf("project settings (HOLE_PROJECT_SETTINGS_FILE %s)", path)
			target.overrideVar = "HOLE_PROJECT_SETTINGS_FILE"
		}
		return target, nil
	}
}

// requireSettingsFile checks that the settings file exists and is a regular file. os.Stat
// follows symlinks, so a dangling link counts as missing. createHint is the `hole settings
// create …` command that would create the file, built from the arguments as the user typed them.
func requireSettingsFile(path, createHint string) error {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("settings file '%s' does not exist; create it with '%s'", path, createHint)
	case err != nil:
		return fmt.Errorf("stat %s: %w", path, err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("'%s' is not a settings file", path)
	}
	return nil
}

func settingsCreate(inv *Invocation, target settingsTarget) int {
	mode := config.TemplateCreate
	switch {
	case inv.Overwrite:
		mode = config.TemplateOverwrite
	case inv.Append:
		mode = config.TemplateAppend
	}
	existed, err := config.WriteSettingsTemplate(target.path, mode)
	if err != nil {
		logging.Error("%v", err)
		return 1
	}
	switch {
	case !existed:
		logging.Info("Created %s", target.path)
	case mode == config.TemplateOverwrite:
		logging.Info("Replaced %s", target.path)
	default:
		logging.Info("Updated %s", target.path)
	}
	return 0
}

func settingsOpen(sio settingsIO, target settingsTarget, createHint string) int {
	if err := requireSettingsFile(target.path, createHint); err != nil {
		logging.Error("%v", err)
		return 1
	}
	editor := editorCommand(os.Getenv, sio.terminal)
	if editor == "" {
		if !sio.terminal {
			logging.Error("no editor configured and no terminal to run vi in; set $EDITOR")
			return 1
		}
		if _, err := exec.LookPath("vi"); err != nil {
			logging.Error("no editor found; set $EDITOR")
			return 1
		}
		editor = "vi"
	}
	for {
		if err := openInEditor(editor, target.path); err != nil {
			logging.Error("%v", err)
			return 1
		}
		// LoadAndValidate treats a missing file as valid, so an editor that deleted or renamed
		// the file must be caught here.
		if err := requireSettingsFile(target.path, createHint); err != nil {
			logging.Error("%v", err)
			return 1
		}
		_, err := config.LoadAndValidate(target.path, target.label)
		if err == nil {
			return 0
		}
		logging.Error("%v", err)
		if !sio.terminal || !confirmReopen(sio.stdin, sio.stderr, target.path) {
			return 1
		}
	}
}

// editorCommand picks the user's editor: $VISUAL (only with a terminal), then $EDITOR. Empty
// means none is configured. Blank values are skipped.
func editorCommand(lookup func(string) string, terminal bool) string {
	if terminal {
		if visual := strings.TrimSpace(lookup("VISUAL")); visual != "" {
			return visual
		}
	}
	return strings.TrimSpace(lookup("EDITOR"))
}

// openInEditor runs the editor on path with the process's stdio attached and waits for it.
// The path travels as "$1", never as part of the script, so a path containing spaces or
// `$(…)` is safe and an editor value with arguments (`code -w`) still works.
func openInEditor(editor, path string) error {
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "hole", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return fmt.Errorf("editor '%s' exited with status %d", editor, exitErr.ExitCode())
	case errors.As(err, &exitErr):
		return fmt.Errorf("editor '%s' was terminated: %w", editor, err)
	default:
		return fmt.Errorf("run editor '%s': %w", editor, err)
	}
}

// confirmReopen asks whether an invalid settings file should go back to the editor. Yes is
// the default; EOF or a read error means no, so a closed stdin cannot loop forever.
func confirmReopen(in *bufio.Reader, out io.Writer, path string) bool {
	_, _ = fmt.Fprintf(out, "Reopen %s in the editor? [Y/n] ", path)
	answer, err := in.ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func settingsGet(sio settingsIO, target settingsTarget, createHint string) int {
	if err := requireSettingsFile(target.path, createHint); err != nil {
		logging.Error("%v", err)
		return 1
	}
	_, _ = fmt.Fprintln(sio.stdout, target.path)
	return 0
}
