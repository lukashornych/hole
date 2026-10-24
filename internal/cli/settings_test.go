package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
)

// isolateSettingsEnv points HOME at a temp directory and clears every variable the settings
// subcommands read, so a developer's environment cannot leak in. It returns HOME.
func isolateSettingsEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"HOLE_SETTINGS_FILE", "HOLE_PROJECT_SETTINGS_FILE", "EDITOR", "VISUAL"} {
		t.Setenv(name, "")
	}
	return home
}

// captureLogs redirects the package logger into a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

// resolvedTempDir is t.TempDir() with symlinks resolved, as ResolveProjectDir returns it
// (macOS keeps temp directories under the /var -> /private/var link).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runSettingsArgs(t *testing.T, sio settingsIO, args ...string) int {
	t.Helper()
	inv, err := Parse(append([]string{"settings"}, args...))
	if err != nil {
		t.Fatalf("Parse(%v): %v", args, err)
	}
	if sio.stdin == nil {
		sio.stdin = bufio.NewReader(strings.NewReader(""))
	}
	if sio.stdout == nil {
		sio.stdout = &bytes.Buffer{}
	}
	if sio.stderr == nil {
		sio.stderr = &bytes.Buffer{}
	}
	return runSettings(inv, sio)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestResolveSettingsTarget(t *testing.T) {
	projectDir := resolvedTempDir(t)
	override := filepath.Join(t.TempDir(), "override.json")
	tests := map[string]struct {
		host        hostenv.Host
		scope       string
		projectArg  string
		want        settingsTarget
		wantErrPart string
	}{
		"global default": {
			host:  hostenv.Host{Home: "/home/dev"},
			scope: "global",
			want:  settingsTarget{path: "/home/dev/.hole/settings.json", label: "global settings (/home/dev/.hole/settings.json)"},
		},
		"global override": {
			host:  hostenv.Host{Home: "/home/dev", GlobalSettingsOverride: override},
			scope: "global",
			want:  settingsTarget{path: override, label: "global settings (" + override + ")", overrideVar: "HOLE_SETTINGS_FILE"},
		},
		"global relative override": {
			host:        hostenv.Host{Home: "/home/dev", GlobalSettingsOverride: "x.json"},
			scope:       "global",
			wantErrPart: "HOLE_SETTINGS_FILE",
		},
		"project default": {
			host:       hostenv.Host{Home: "/home/dev"},
			scope:      "project",
			projectArg: projectDir,
			want:       settingsTarget{path: filepath.Join(projectDir, ".hole", "settings.json"), label: "project settings (.hole/settings.json)"},
		},
		"project override": {
			host:       hostenv.Host{Home: "/home/dev", ProjectSettingsOverride: override},
			scope:      "project",
			projectArg: projectDir,
			want: settingsTarget{path: override, label: "project settings (HOLE_PROJECT_SETTINGS_FILE " + override + ")",
				overrideVar: "HOLE_PROJECT_SETTINGS_FILE"},
		},
		"project relative override": {
			host:        hostenv.Host{Home: "/home/dev", ProjectSettingsOverride: "p.json"},
			scope:       "project",
			projectArg:  projectDir,
			wantErrPart: "HOLE_PROJECT_SETTINGS_FILE",
		},
		"project missing directory despite override": {
			host:        hostenv.Host{Home: "/home/dev", ProjectSettingsOverride: override},
			scope:       "project",
			projectArg:  filepath.Join(projectDir, "missing"),
			wantErrPart: "does not exist",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := resolveSettingsTarget(test.host, test.scope, test.projectArg)
			if test.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
					t.Fatalf("error = %v, want one containing %q", err, test.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSettingsTarget() error = %v", err)
			}
			if got != test.want {
				t.Errorf("resolveSettingsTarget() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestRequireSettingsFile(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(regular, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "nowhere.json"), dangling); err != nil {
		t.Fatal(err)
	}
	const hint = "hole settings create project ."
	tests := map[string]struct {
		path      string
		wantParts []string
	}{
		"regular":   {path: regular},
		"missing":   {path: filepath.Join(dir, "missing.json"), wantParts: []string{"does not exist", hint}},
		"dangling":  {path: dangling, wantParts: []string{"does not exist", hint}},
		"directory": {path: dir, wantParts: []string{"is not a settings file"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := requireSettingsFile(test.path, hint)
			if len(test.wantParts) == 0 {
				if err != nil {
					t.Errorf("requireSettingsFile() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("requireSettingsFile() succeeded")
			}
			for _, want := range append(test.wantParts, test.path) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestEditorCommand(t *testing.T) {
	tests := map[string]struct {
		env      map[string]string
		terminal bool
		want     string
	}{
		"visual wins with a terminal":       {env: map[string]string{"VISUAL": "vim", "EDITOR": "nano"}, terminal: true, want: "vim"},
		"visual ignored without a terminal": {env: map[string]string{"VISUAL": "vim", "EDITOR": "nano"}, want: "nano"},
		"blank visual skipped":              {env: map[string]string{"VISUAL": "  ", "EDITOR": "nano"}, terminal: true, want: "nano"},
		"blank editor":                      {env: map[string]string{"EDITOR": " "}, terminal: true},
		"nothing set":                       {terminal: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			lookup := func(key string) string { return test.env[key] }
			if got := editorCommand(lookup, test.terminal); got != test.want {
				t.Errorf("editorCommand() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOpenInEditorPassesPathAsArgument(t *testing.T) {
	dir := t.TempDir()
	editor := filepath.Join(dir, "editor.sh")
	writeScript(t, editor, `printf x >> "$1"`+"\n")
	path := filepath.Join(dir, "a b $(touch pwned).json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := openInEditor(editor, path); err != nil {
		t.Fatalf("openInEditor() error = %v", err)
	}
	if content, _ := os.ReadFile(path); string(content) != "x" {
		t.Errorf("file content = %q, want x", content)
	}
	if exists(filepath.Join(dir, "pwned")) {
		t.Error("the path was evaluated by the shell")
	}
}

func TestOpenInEditorReportsExitStatus(t *testing.T) {
	err := openInEditor("false", filepath.Join(t.TempDir(), "settings.json"))
	if err == nil || !strings.Contains(err.Error(), "status 1") {
		t.Errorf("openInEditor(false) error = %v, want one naming status 1", err)
	}
}

func TestConfirmReopen(t *testing.T) {
	tests := map[string]bool{
		"\n": true, "y\n": true, "Y\n": true, "yes\n": true,
		"n\n": false, "no\n": false, "x\n": false, "": false,
	}
	for input, want := range tests {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			var out bytes.Buffer
			got := confirmReopen(bufio.NewReader(strings.NewReader(input)), &out, "/p/settings.json")
			if got != want {
				t.Errorf("confirmReopen(%q) = %v, want %v", input, got, want)
			}
			if !strings.Contains(out.String(), "/p/settings.json") {
				t.Errorf("prompt %q does not name the path", out.String())
			}
		})
	}
}

// recordingEditor installs an editor script that appends every path it is given to a log file,
// and returns that log file.
func recordingEditor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	editor := filepath.Join(dir, "editor.sh")
	writeScript(t, editor, fmt.Sprintf("echo \"$1\" >> '%s'\n", calls))
	t.Setenv("EDITOR", editor)
	return calls
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	content, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(content))
}

func TestSettingsOpenRequiresExistingFile(t *testing.T) {
	home := isolateSettingsEnv(t)
	calls := recordingEditor(t)
	logs := captureLogs(t)

	if code := runSettingsArgs(t, settingsIO{}, "open", "global"); code != 1 {
		t.Fatalf("open global without a file = %d, want 1", code)
	}
	if got := readCalls(t, calls); len(got) != 0 {
		t.Errorf("the editor ran on %v", got)
	}
	if exists(filepath.Join(home, ".hole")) {
		t.Error("open created ~/.hole")
	}
	for _, want := range []string{filepath.Join(home, ".hole", "settings.json"), "hole settings create global"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs do not mention %q:\n%s", want, logs.String())
		}
	}

	if code := runSettingsArgs(t, settingsIO{}, "create", "global"); code != 0 {
		t.Fatalf("create global = %d", code)
	}
	if code := runSettingsArgs(t, settingsIO{}, "open", "global"); code != 0 {
		t.Fatalf("open global = %d, want 0", code)
	}
	if got := readCalls(t, calls); len(got) != 1 || got[0] != filepath.Join(home, ".hole", "settings.json") {
		t.Errorf("editor calls = %v", got)
	}
}

func TestSettingsOpenDoesNotCreateMissingOverride(t *testing.T) {
	isolateSettingsEnv(t)
	recordingEditor(t)
	override := filepath.Join(t.TempDir(), "x.json")
	t.Setenv("HOLE_SETTINGS_FILE", override)
	if code := runSettingsArgs(t, settingsIO{}, "open", "global"); code != 1 {
		t.Fatalf("open global = %d, want 1", code)
	}
	if exists(override) {
		t.Error("open created the override file")
	}
}

func TestSettingsOpenFailsWhenEditorDeletesFile(t *testing.T) {
	isolateSettingsEnv(t)
	editor := filepath.Join(t.TempDir(), "editor.sh")
	writeScript(t, editor, `rm "$1"`+"\n")
	t.Setenv("EDITOR", editor)
	logs := captureLogs(t)
	if code := runSettingsArgs(t, settingsIO{}, "create", "global"); code != 0 {
		t.Fatalf("create global = %d", code)
	}
	var stderr bytes.Buffer
	sio := settingsIO{stdin: bufio.NewReader(strings.NewReader("\n")), stderr: &stderr, terminal: true}
	if code := runSettingsArgs(t, sio, "open", "global"); code != 1 {
		t.Fatalf("open global = %d, want 1", code)
	}
	if !strings.Contains(logs.String(), "does not exist") {
		t.Errorf("logs do not report the missing file:\n%s", logs.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected prompt %q", stderr.String())
	}
}

// TestSettingsOpenReopenLoop drives the invalid-file loop with an editor that writes an invalid
// document on its first call and, unless alwaysInvalid, the template afterwards.
func TestSettingsOpenReopenLoop(t *testing.T) {
	tests := map[string]struct {
		input         string
		terminal      bool
		alwaysInvalid bool
		wantCode      int
		wantCalls     int
		wantPrompt    bool
	}{
		"reopen and fix":        {input: "\n", terminal: true, wantCode: 0, wantCalls: 2, wantPrompt: true},
		"decline":               {input: "n\n", terminal: true, wantCode: 1, wantCalls: 1, wantPrompt: true},
		"no terminal":           {input: "\n", wantCode: 1, wantCalls: 1},
		"EOF ends the loop":     {input: "\n\n", terminal: true, alwaysInvalid: true, wantCode: 1, wantCalls: 3, wantPrompt: true},
		"invalid stays on disk": {input: "no\n", terminal: true, wantCode: 1, wantCalls: 1, wantPrompt: true},
		"explicit yes":          {input: "y\n", terminal: true, wantCode: 0, wantCalls: 2, wantPrompt: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := isolateSettingsEnv(t)
			scriptDir := t.TempDir()
			counter := filepath.Join(scriptDir, "counter")
			template := filepath.Join(scriptDir, "template.json")
			if err := os.WriteFile(template, config.SettingsTemplate(), 0o644); err != nil {
				t.Fatal(err)
			}
			fixAfter := 1
			if test.alwaysInvalid {
				fixAfter = 1000
			}
			editor := filepath.Join(scriptDir, "editor.sh")
			writeScript(t, editor, fmt.Sprintf(`n=$(cat '%[1]s' 2>/dev/null || echo 0)
n=$((n+1))
echo $n > '%[1]s'
if [ $n -le %[3]d ]; then printf '{"bogus": 1}' > "$1"; else cat '%[2]s' > "$1"; fi
`, counter, template, fixAfter))
			t.Setenv("EDITOR", editor)
			logs := captureLogs(t)
			if code := runSettingsArgs(t, settingsIO{}, "create", "global"); code != 0 {
				t.Fatalf("create global = %d", code)
			}

			var stderr bytes.Buffer
			sio := settingsIO{stdin: bufio.NewReader(strings.NewReader(test.input)), stderr: &stderr, terminal: test.terminal}
			if code := runSettingsArgs(t, sio, "open", "global"); code != test.wantCode {
				t.Errorf("open global = %d, want %d", code, test.wantCode)
			}
			count, _ := os.ReadFile(counter)
			if got := strings.TrimSpace(string(count)); got != fmt.Sprint(test.wantCalls) {
				t.Errorf("editor ran %s times, want %d", got, test.wantCalls)
			}
			if got := strings.Contains(stderr.String(), "Reopen"); got != test.wantPrompt {
				t.Errorf("prompt shown = %v, want %v (%q)", got, test.wantPrompt, stderr.String())
			}
			path := filepath.Join(home, ".hole", "settings.json")
			if !strings.Contains(logs.String(), "global settings ("+path+") is not valid") {
				t.Errorf("logs do not carry the start label:\n%s", logs.String())
			}
			if test.wantCode == 1 {
				if content, _ := os.ReadFile(path); string(content) != `{"bogus": 1}` {
					t.Errorf("file = %q, want the invalid document kept", content)
				}
			}
		})
	}
}

func TestSettingsGet(t *testing.T) {
	home := isolateSettingsEnv(t)
	globalFile := filepath.Join(home, ".hole", "settings.json")

	get := func(args ...string) (int, string) {
		var stdout bytes.Buffer
		code := runSettingsArgs(t, settingsIO{stdout: &stdout}, append([]string{"get"}, args...)...)
		return code, stdout.String()
	}

	if code, out := get("global"); code != 1 || out != "" {
		t.Errorf("get global without a file = %d, %q", code, out)
	}
	if code := runSettingsArgs(t, settingsIO{}, "create", "global"); code != 0 {
		t.Fatalf("create global = %d", code)
	}
	if code, out := get("global"); code != 0 || out != globalFile+"\n" {
		t.Errorf("get global = %d, %q, want %q", code, out, globalFile+"\n")
	}
	if code, _ := get("global", "--append"); code != 1 {
		t.Errorf("get global --append = %d, want 1", code)
	}
	if code, _ := get("global", "extra"); code != 1 {
		t.Errorf("get global extra = %d, want 1", code)
	}

	projectDir := resolvedTempDir(t)
	projectFile := filepath.Join(projectDir, ".hole", "settings.json")
	if code := runSettingsArgs(t, settingsIO{}, "create", "project", projectDir); code != 0 {
		t.Fatalf("create project = %d", code)
	}
	if code, out := get("project", projectDir); code != 0 || out != projectFile+"\n" {
		t.Errorf("get project = %d, %q, want %q", code, out, projectFile+"\n")
	}
	t.Chdir(projectDir)
	if code, out := get("project", "."); code != 0 || out != projectFile+"\n" {
		t.Errorf("get project . = %d, %q, want %q", code, out, projectFile+"\n")
	}

	override := filepath.Join(t.TempDir(), "x.json")
	t.Setenv("HOLE_SETTINGS_FILE", override)
	if code, out := get("global"); code != 1 || out != "" {
		t.Errorf("get global with a missing override = %d, %q", code, out)
	}
	if err := os.WriteFile(override, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := get("global"); code != 0 || out != override+"\n" {
		t.Errorf("get global with an override = %d, %q", code, out)
	}

	projectOverride := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(projectOverride, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLE_PROJECT_SETTINGS_FILE", projectOverride)
	if code, out := get("project", projectDir); code != 0 || out != projectOverride+"\n" {
		t.Errorf("get project with an override = %d, %q", code, out)
	}

	t.Setenv("HOLE_PROJECT_SETTINGS_FILE", filepath.Dir(projectOverride))
	if code, out := get("project", projectDir); code != 1 || out != "" {
		t.Errorf("get project on a directory = %d, %q", code, out)
	}
}
