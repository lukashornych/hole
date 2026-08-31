package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/state"
)

// TestWriteDumpFileStaysInHoleDir pins finding 4's fix: the network-access dump is written
// under ~/.hole/logs/<project>/, never into the project's own .hole/logs. The project
// directory is bind-mounted read-write with the host UID, so writing there let the sandbox
// pre-plant a symlink and redirect this host-side write anywhere the user can write.
func TestWriteDumpFileStaysInHoleDir(t *testing.T) {
	home := t.TempDir()
	host := hostenv.Host{Home: home, Username: "dev"}
	project := t.TempDir()
	instance := &state.Instance{
		ProjectPath: project,
		ProjectName: "proj-0badf00d",
		Agent:       "claude",
		InstanceID:  "abc123",
	}

	path, err := writeDumpFile(host, instance, "ALLOWED example.com\n")
	if err != nil {
		t.Fatalf("writeDumpFile: %v", err)
	}

	want := filepath.Join(host.LogDir(), instance.ProjectName, "network-access-claude-abc123.log")
	if path != want {
		t.Fatalf("dump path = %q, want %q", path, want)
	}
	if data, _ := os.ReadFile(path); string(data) != "ALLOWED example.com\n" {
		t.Fatalf("dump content = %q", string(data))
	}
	if _, err := os.Lstat(filepath.Join(project, ".hole")); !os.IsNotExist(err) {
		t.Fatalf("dump must not create anything under the project dir, got err=%v", err)
	}
}

// TestWriteDumpFileDoesNotFollowSymlink proves the write target is unreachable to a sandbox
// symlink: even if an attacker plants a directory symlink where the old code wrote
// (<project>/.hole/logs -> victim), the dump lands in ~/.hole and the victim is untouched.
func TestWriteDumpFileDoesNotFollowSymlink(t *testing.T) {
	home := t.TempDir()
	host := hostenv.Host{Home: home, Username: "dev"}
	project := t.TempDir()

	victim := t.TempDir()
	secret := filepath.Join(victim, "authorized_keys")
	if err := os.WriteFile(secret, []byte("REAL-KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The pre-2.x write target, now unused, is where the sandbox could plant a symlink.
	if err := os.MkdirAll(filepath.Join(project, ".hole"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(project, ".hole", "logs")); err != nil {
		t.Fatal(err)
	}

	instance := &state.Instance{ProjectPath: project, ProjectName: "proj-0badf00d", Agent: "claude", InstanceID: "abc123"}
	if _, err := writeDumpFile(host, instance, "ALLOWED attacker.example\n"); err != nil {
		t.Fatalf("writeDumpFile: %v", err)
	}

	if data, _ := os.ReadFile(secret); string(data) != "REAL-KEY\n" {
		t.Fatalf("victim file was overwritten: %q", string(data))
	}
}
