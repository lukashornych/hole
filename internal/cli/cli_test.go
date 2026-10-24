package cli

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseFlagsAndPositionalsInterleave(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		command    string
		positional []string
		agentArgs  []string
		debug      bool
		dump       bool
		rebuild    bool
		unrestrict bool
		withDocker bool
		trust      bool
		update     bool
	}{
		{
			name:       "plain start",
			args:       []string{"start", "claude", "."},
			command:    "start",
			positional: []string{"claude", "."},
		},
		{
			name:       "flag before positionals",
			args:       []string{"start", "-d", "claude", "."},
			command:    "start",
			positional: []string{"claude", "."},
			debug:      true,
		},
		{
			name:       "flag between positionals",
			args:       []string{"start", "claude", "-r", "."},
			command:    "start",
			positional: []string{"claude", "."},
			rebuild:    true,
		},
		{
			name:       "flag after positionals",
			args:       []string{"start", "claude", ".", "--with-docker"},
			command:    "start",
			positional: []string{"claude", "."},
			withDocker: true,
		},
		{
			name:       "all flags",
			args:       []string{"start", "claude", ".", "-n", "-r", "-u", "--with-docker", "--trust-project", "--update-agents"},
			command:    "start",
			positional: []string{"claude", "."},
			dump:       true,
			rebuild:    true,
			unrestrict: true,
			withDocker: true,
			trust:      true,
			update:     true,
		},
		{
			name:       "long flag forms",
			args:       []string{"start", "claude", ".", "--debug", "--dump-network-access", "--rebuild", "--unrestricted-network"},
			command:    "start",
			positional: []string{"claude", "."},
			debug:      true,
			dump:       true,
			rebuild:    true,
			unrestrict: true,
		},
		{
			name:       "agent args after separator",
			args:       []string{"start", "claude", ".", "--", "-p", "explain this"},
			command:    "start",
			positional: []string{"claude", "."},
			agentArgs:  []string{"-p", "explain this"},
		},
		{
			name:       "hole flags after separator belong to the agent",
			args:       []string{"start", "claude", ".", "--", "-r", "--debug", "--"},
			command:    "start",
			positional: []string{"claude", "."},
			agentArgs:  []string{"-r", "--debug", "--"},
		},
		{
			name:       "destroy with path",
			args:       []string{"destroy", "/path/to/project"},
			command:    "destroy",
			positional: []string{"/path/to/project"},
		},
		{
			name:    "destroy without path",
			args:    []string{"destroy"},
			command: "destroy",
		},
		{
			name:    "no arguments",
			args:    nil,
			command: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inv, err := Parse(test.args)
			if err != nil {
				t.Fatalf("Parse(%v): %v", test.args, err)
			}
			if inv.Command != test.command {
				t.Errorf("command = %q, want %q", inv.Command, test.command)
			}
			if !equalStrings(inv.Positional, test.positional) {
				t.Errorf("positional = %v, want %v", inv.Positional, test.positional)
			}
			if !equalStrings(inv.AgentArgs, test.agentArgs) {
				t.Errorf("agent args = %v, want %v", inv.AgentArgs, test.agentArgs)
			}
			if inv.Debug != test.debug || inv.DumpNetworkAccess != test.dump || inv.Rebuild != test.rebuild ||
				inv.Unrestricted != test.unrestrict || inv.WithDocker != test.withDocker ||
				inv.TrustProject != test.trust || inv.UpdateAgents != test.update {
				t.Errorf("flags = %+v", inv)
			}
		})
	}
}

func TestParseRejectsUnknownOptions(t *testing.T) {
	for _, args := range [][]string{
		{"start", "claude", ".", "-x"},
		{"start", "claude", ".", "--nope"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%v) accepted an unknown option", args)
		}
	}
}

func TestParseKeepsBareDashAsPositional(t *testing.T) {
	inv, err := Parse([]string{"start", "claude", "-"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(inv.Positional, []string{"claude", "-"}) {
		t.Errorf("positional = %v", inv.Positional)
	}
}

func TestStartRequiresProjectPath(t *testing.T) {
	// Bug fix: `hole start claude` used to silently default to the current directory.
	if code := Run([]string{"start", "claude"}); code == 0 {
		t.Error("start without a project path must fail")
	}
}

func TestStartRejectsExtraPositional(t *testing.T) {
	if code := Run([]string{"start", "claude", ".", "extra"}); code == 0 {
		t.Error("start with a third positional must fail")
	}
}

func TestStartRejectsDebugWithAgentArgs(t *testing.T) {
	if code := Run([]string{"start", "claude", ".", "-d", "--", "-p", "x"}); code == 0 {
		t.Error("--debug together with agent arguments must fail")
	}
}

func TestDestroyRejectsExtraPositional(t *testing.T) {
	// Bug fix: `hole destroy <path>` used to land the path in the agent slot.
	if code := Run([]string{"destroy", "/a", "/b"}); code == 0 {
		t.Error("destroy with two paths must fail")
	}
}

func TestDestroyWithMissingPathFails(t *testing.T) {
	if code := Run([]string{"destroy", "/definitely/not/here"}); code == 0 {
		t.Error("destroy of a nonexistent path must fail")
	}
}

func TestInvalidCommandFails(t *testing.T) {
	if code := Run([]string{"bogus"}); code == 0 {
		t.Error("unknown command must fail")
	}
}

func TestHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{{"help"}, {}} {
		if code := Run(args); code != 0 {
			t.Errorf("Run(%v) = %d, want 0", args, code)
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func TestStartRejectsInvalidProfileNames(t *testing.T) {
	// The agent positional splits on the first colon, so these all fail on the profile part
	// rather than reaching the agent registry.
	for _, agent := range []string{"claude:", "claude:Foo", "claude:a:b", "claude:a,b", "claude:-x"} {
		if code := Run([]string{"start", agent, "."}); code == 0 {
			t.Errorf("start %s should have failed on the profile name", agent)
		}
	}
}

func TestProfileIsRejectedForOtherCommands(t *testing.T) {
	// A profile selects a settings overlay for one sandbox; anywhere else it is a typo.
	for _, args := range [][]string{
		{"destroy", "claude:research"},
		{"list", "claude:research"},
		{"version", "claude:research"},
	} {
		if code := Run(args); code == 0 {
			t.Errorf("%v should have been rejected", args)
		}
	}
}

func TestListRejectsMalformedInstanceID(t *testing.T) {
	if code := Run([]string{"list", "extra"}); code == 0 {
		t.Error("hole list must reject a positional that is not an instance ID")
	}
}

func TestListRejectsTwoPositionals(t *testing.T) {
	if code := Run([]string{"list", "--json", "r8tq2c", "abc123"}); code == 0 {
		t.Error("hole list takes at most one instance ID")
	}
}

func TestParseInstanceIDForms(t *testing.T) {
	for _, args := range [][]string{
		{"start", "claude", ".", "--instance-id", "r8tq2c"},
		{"start", "claude", ".", "--instance-id=r8tq2c"},
		{"start", "--instance-id", "r8tq2c", "claude", "."},
	} {
		inv, err := Parse(args)
		if err != nil {
			t.Fatalf("Parse(%v): %v", args, err)
		}
		if inv.InstanceID != "r8tq2c" {
			t.Errorf("Parse(%v).InstanceID = %q, want r8tq2c", args, inv.InstanceID)
		}
		if !reflect.DeepEqual(inv.Positional, []string{"claude", "."}) {
			t.Errorf("Parse(%v).Positional = %v", args, inv.Positional)
		}
	}
}

func TestParseInstanceIDWithoutValueFails(t *testing.T) {
	_, err := Parse([]string{"start", "claude", ".", "--instance-id"})
	if err == nil || err.Error() != "--instance-id needs a value: 6 characters a-z, 0-9" {
		t.Errorf("Parse error = %v", err)
	}
}

func TestParseInstanceIDRejectsBadFormat(t *testing.T) {
	for _, args := range [][]string{
		{"start", "claude", ".", "--instance-id", "R8TQ2C"},
		{"start", "claude", ".", "--instance-id=R8TQ2C"},
	} {
		_, err := Parse(args)
		if err == nil || err.Error() != "invalid instance ID 'R8TQ2C': expected 6 characters a-z, 0-9" {
			t.Errorf("Parse(%v) error = %v", args, err)
		}
	}
}

func TestParseInstanceIDTwiceLastWins(t *testing.T) {
	inv, err := Parse([]string{"start", "claude", ".", "--instance-id", "aaaaaa", "--instance-id=bbbbbb"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if inv.InstanceID != "bbbbbb" {
		t.Errorf("InstanceID = %q, want bbbbbb", inv.InstanceID)
	}
}

func TestParseInstanceIDAfterSeparatorBelongsToTheAgent(t *testing.T) {
	inv, err := Parse([]string{"start", "claude", ".", "--", "--instance-id", "x", "--json", "--no-gc"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if inv.InstanceID != "" || inv.JSON || inv.NoGC {
		t.Errorf("flags after -- were parsed by hole: %+v", inv)
	}
}

func TestInstanceIDIsRejectedForOtherCommands(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--instance-id", "r8tq2c"},
		{"destroy", "--instance-id", "r8tq2c"},
	} {
		if code := Run(args); code == 0 {
			t.Errorf("%v should have been rejected", args)
		}
	}
}

func TestParseJSONAndNoGC(t *testing.T) {
	inv, err := Parse([]string{"list", "--json", "--no-gc", "r8tq2c"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !inv.JSON || !inv.NoGC {
		t.Errorf("JSON = %v, NoGC = %v, want both true", inv.JSON, inv.NoGC)
	}
	if !reflect.DeepEqual(inv.Positional, []string{"r8tq2c"}) {
		t.Errorf("positional = %v", inv.Positional)
	}
}

func TestJSONIsRejectedForOtherCommands(t *testing.T) {
	for _, args := range [][]string{
		{"start", "claude", ".", "--json"},
		{"destroy", "--json"},
	} {
		if code := Run(args); code == 0 {
			t.Errorf("%v should have been rejected", args)
		}
	}
}

func TestNoGCIsRejectedForOtherCommands(t *testing.T) {
	for _, args := range [][]string{
		{"start", "claude", ".", "--no-gc"},
		{"destroy", "--no-gc"},
	} {
		if code := Run(args); code == 0 {
			t.Errorf("%v should have been rejected", args)
		}
	}
}

func TestHelpDocumentsProfilesAndList(t *testing.T) {
	for _, want := range []string{"{agent}[:{profile}]", "hole list", "profiles", "--instance-id", "--json", "--no-gc"} {
		if !strings.Contains(helpText, want) {
			t.Errorf("help text is missing %q", want)
		}
	}
}

func TestParseLibraryFlagForms(t *testing.T) {
	inv, err := Parse([]string{
		"start", "claude", ".",
		"--library", "/host/a",
		"--library=/host/b:/libs/b:rw",
		"--library", "~/c:rw",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"/host/a", "/host/b:/libs/b:rw", "~/c:rw"}
	if !reflect.DeepEqual(inv.Libraries, want) {
		t.Errorf("libraries = %v, want %v", inv.Libraries, want)
	}
	// The values must not leak into the positionals.
	if !reflect.DeepEqual(inv.Positional, []string{"claude", "."}) {
		t.Errorf("positional = %v", inv.Positional)
	}
}

func TestParseLibraryWithoutValueFails(t *testing.T) {
	if _, err := Parse([]string{"start", "claude", ".", "--library"}); err == nil {
		t.Error("--library without a value must fail")
	}
}

func TestParseLibraryAfterSeparatorBelongsToTheAgent(t *testing.T) {
	inv, err := Parse([]string{"start", "claude", ".", "--", "--library", "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Libraries) != 0 {
		t.Errorf("libraries = %v, want none", inv.Libraries)
	}
	if !reflect.DeepEqual(inv.AgentArgs, []string{"--library", "/x"}) {
		t.Errorf("agent args = %v", inv.AgentArgs)
	}
}

func TestParseSettingsFlags(t *testing.T) {
	tests := []struct {
		args       []string
		positional []string
		overwrite  bool
		appendFlag bool
	}{
		{args: []string{"settings", "create", "project", ".", "--append"}, positional: []string{"create", "project", "."}, appendFlag: true},
		{args: []string{"--overwrite", "settings", "create", "global"}, positional: []string{"create", "global"}, overwrite: true},
	}
	for _, test := range tests {
		inv, err := Parse(test.args)
		if err != nil {
			t.Fatalf("Parse(%v): %v", test.args, err)
		}
		if inv.Command != "settings" || !reflect.DeepEqual(inv.Positional, test.positional) {
			t.Errorf("Parse(%v) = %q %v", test.args, inv.Command, inv.Positional)
		}
		if inv.Overwrite != test.overwrite || inv.Append != test.appendFlag {
			t.Errorf("Parse(%v): Overwrite = %v, Append = %v", test.args, inv.Overwrite, inv.Append)
		}
	}
}

func TestSettingsRejectsInvalidInvocations(t *testing.T) {
	isolateSettingsEnv(t)
	for _, args := range [][]string{
		{"start", "claude", ".", "--overwrite"},
		{"list", "--append"},
		{"settings", "create", "global", "--overwrite", "--append"},
		{"settings", "open", "global", "--overwrite"},
		{"settings", "create", "global", "/tmp"},
		{"settings", "create", "project"},
		{"settings", "create", "project", "/does/not/exist"},
		{"settings", "create", "workspace", "."},
		{"settings", "delete", "global"},
		{"settings"},
	} {
		if code := Run(args); code == 0 {
			t.Errorf("%v should have been rejected", args)
		}
	}
}

func TestSettingsCreateModes(t *testing.T) {
	home := isolateSettingsEnv(t)
	globalFile := filepath.Join(home, ".hole", "settings.json")
	steps := []struct {
		args []string
		want int
	}{
		{args: []string{"settings", "create", "global"}, want: 0},
		{args: []string{"settings", "create", "global"}, want: 1},
		{args: []string{"settings", "create", "global", "--append"}, want: 0},
		{args: []string{"settings", "create", "global", "--overwrite"}, want: 0},
	}
	for _, step := range steps {
		if code := Run(step.args); code != step.want {
			t.Errorf("%v = %d, want %d", step.args, code, step.want)
		}
		if !exists(globalFile) {
			t.Errorf("%s is missing after %v", globalFile, step.args)
		}
	}

	projectDir := t.TempDir()
	if code := Run([]string{"settings", "create", "project", projectDir}); code != 0 {
		t.Errorf("settings create project = %d", code)
	}
	if !exists(filepath.Join(projectDir, ".hole", "settings.json")) {
		t.Error("settings create project did not create .hole/settings.json")
	}
}

func TestSettingsCreateFollowsOverrides(t *testing.T) {
	home := isolateSettingsEnv(t)
	overrideDir := t.TempDir()

	globalOverride := filepath.Join(overrideDir, "x.json")
	t.Setenv("HOLE_SETTINGS_FILE", globalOverride)
	if code := Run([]string{"settings", "create", "global"}); code != 0 {
		t.Errorf("settings create global = %d", code)
	}
	if !exists(globalOverride) || exists(filepath.Join(home, ".hole", "settings.json")) {
		t.Error("settings create global did not follow HOLE_SETTINGS_FILE")
	}

	projectDir := t.TempDir()
	projectOverride := filepath.Join(overrideDir, "p.json")
	t.Setenv("HOLE_PROJECT_SETTINGS_FILE", projectOverride)
	if code := Run([]string{"settings", "create", "project", projectDir}); code != 0 {
		t.Errorf("settings create project = %d", code)
	}
	if !exists(projectOverride) || exists(filepath.Join(projectDir, ".hole")) {
		t.Error("settings create project did not follow HOLE_PROJECT_SETTINGS_FILE")
	}
}

func TestSettingsCreateRejectsRelativeOverride(t *testing.T) {
	isolateSettingsEnv(t)
	t.Setenv("HOLE_SETTINGS_FILE", "x.json")
	logs := captureLogs(t)
	if code := Run([]string{"settings", "create", "global"}); code != 1 {
		t.Errorf("settings create global = %d, want 1", code)
	}
	if !strings.Contains(logs.String(), "HOLE_SETTINGS_FILE") {
		t.Errorf("logs do not name the variable:\n%s", logs.String())
	}
}

func TestHelpDocumentsSettings(t *testing.T) {
	for _, want := range []string{"hole settings", "--overwrite", "--append", "settings create project ."} {
		if !strings.Contains(helpText, want) {
			t.Errorf("help text is missing %q", want)
		}
	}
}
