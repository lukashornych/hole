package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lukashornych/hole/v2/internal/agents"
)

func TestRenderBareWrapper(t *testing.T) {
	for _, test := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"plain path", []string{"/usr/bin/node", "/opt/cli"}, `exec '/usr/bin/node' '/opt/cli' "$@"`},
		{"$HOME", []string{"$HOME/bin/node"}, `exec '/home/u/bin/node' "$@"`},
		{"${HOME}", []string{"${HOME}/bin/node"}, `exec '/home/u/bin/node' "$@"`},
		{"space", []string{"/opt/my cli"}, `exec '/opt/my cli' "$@"`},
		{"single quote", []string{"/opt/it's"}, `exec '/opt/it'\''s' "$@"`},
		{"other variable stays literal", []string{"$OTHER/bin", "${PATH}"}, `exec '$OTHER/bin' '${PATH}' "$@"`},
		// The same textual replacement as command.json: $HOME is a prefix match, not a word.
		{"$HOME prefix", []string{"$HOMEDIR"}, `exec '/home/uDIR' "$@"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := "#!/bin/sh\n" + test.want + "\n"
			if got := string(renderBareWrapper(test.parts, "/home/u")); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// The wrapper must hand its arguments to the target untouched and return the target's exit code.
func TestBareWrapperPassesArgumentsAndExitCode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "it's a target")
	writeScript(t, target, "#!/bin/sh\nfor arg in \"$@\"; do printf '<%s>\\n' \"$arg\"; done\nexit 3\n")
	wrapper := filepath.Join(dir, "wrapper")
	if err := os.WriteFile(wrapper, renderBareWrapper([]string{"$HOME/it's a target", "--fixed"}, dir), 0o755); err != nil {
		t.Fatal(err)
	}

	output, err := exec.Command("sh", "-c", `"$0" "a b" "c'd" '$HOME' ''`, wrapper).Output()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("wrapper exited with %v, want exit code 3", err)
	}
	want := "<--fixed>\n<a b>\n<c'd>\n<$HOME>\n<>\n"
	if string(output) != want {
		t.Errorf("target received %q, want %q", output, want)
	}
}

func TestMaterializeBuildContextWritesBareWrappers(t *testing.T) {
	withBare := &agents.Agent{Name: "with-bare", Source: agents.SourceUser, Dir: fstest.MapFS{
		"command-bare.json": {Data: []byte(`["$HOME/node", "$HOME/cli"]`)},
		"install-user.sh":   {Data: []byte("#!/bin/sh\n")},
	}}
	withoutBare := &agents.Agent{Name: "without-bare", Source: agents.SourceUser, Dir: fstest.MapFS{
		"install-user.sh": {Data: []byte("#!/bin/sh\n")},
	}}

	contextDir, err := materializeBuildContext(t.TempDir(), []*agents.Agent{withBare, withoutBare}, nil, "/home/u")
	if err != nil {
		t.Fatal(err)
	}

	wrapper := filepath.Join(contextDir, "agent-installs", "with-bare", "bare-command")
	info, err := os.Stat(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("wrapper mode %v, want 0755", info.Mode().Perm())
	}
	if content, _ := os.ReadFile(wrapper); string(content) != "#!/bin/sh\nexec '/home/u/node' '/home/u/cli' \"$@\"\n" {
		t.Errorf("wrapper content %q", content)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "agent-installs", "without-bare", "bare-command")); !os.IsNotExist(err) {
		t.Errorf("an agent without command-bare.json got a wrapper: %v", err)
	}
}

func TestMaterializeBuildContextRejectsAnInvalidBareCommand(t *testing.T) {
	broken := &agents.Agent{Name: "broken", Source: agents.SourceUser, Dir: fstest.MapFS{
		"command-bare.json": {Data: []byte(`[]`)},
	}}
	_, err := materializeBuildContext(t.TempDir(), []*agents.Agent{broken}, nil, "/home/u")
	if err == nil || !strings.Contains(err.Error(), "command-bare.json") {
		t.Errorf("error %v, want one naming command-bare.json", err)
	}
}
