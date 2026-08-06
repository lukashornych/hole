package cli

import (
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
			args:       []string{"start", "claude", ".", "-n", "-r", "-u", "--with-docker", "--trust-project"},
			command:    "start",
			positional: []string{"claude", "."},
			dump:       true,
			rebuild:    true,
			unrestrict: true,
			withDocker: true,
			trust:      true,
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
				inv.TrustProject != test.trust {
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

func TestListRejectsPositionalArguments(t *testing.T) {
	if code := Run([]string{"list", "extra"}); code == 0 {
		t.Error("hole list takes no arguments")
	}
}

func TestHelpDocumentsProfilesAndList(t *testing.T) {
	for _, want := range []string{"{agent}[:{profile}]", "hole list", "profiles"} {
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
