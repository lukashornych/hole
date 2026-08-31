package hostenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandEnvVars(t *testing.T) {
	t.Setenv("HOLE_TEST_ONE", "one")
	t.Setenv("HOLE_TEST_TWO", "two")

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"braced", "${HOLE_TEST_ONE}/x", "one/x"},
		{"bare", "$HOLE_TEST_ONE/x", "one/x"},
		{"repeated", "$HOLE_TEST_ONE/$HOLE_TEST_ONE", "one/one"},
		{"two vars", "${HOLE_TEST_ONE}-${HOLE_TEST_TWO}", "one-two"},
		{"undefined braced stays literal", "${HOLE_TEST_MISSING}/x", "${HOLE_TEST_MISSING}/x"},
		{"undefined bare stays literal", "$HOLE_TEST_MISSING/x", "$HOLE_TEST_MISSING/x"},
		{"mixed defined and undefined", "$HOLE_TEST_ONE/$HOLE_TEST_MISSING", "one/$HOLE_TEST_MISSING"},
		{"no variables", "/plain/path", "/plain/path"},
		{"dollar without name", "/a$/b", "/a$/b"},
		// A variable whose value looks like another reference must not be re-expanded:
		// re-scanning is what made the bash implementation loop.
		{"value is not rescanned", "${HOLE_TEST_SELF}", "${HOLE_TEST_SELF}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ExpandEnvVars(test.input); got != test.want {
				t.Errorf("ExpandEnvVars(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestExpandEnvVarsDoesNotRescanValues(t *testing.T) {
	t.Setenv("HOLE_OUTER", "$HOLE_INNER")
	t.Setenv("HOLE_INNER", "boom")
	if got := ExpandEnvVars("${HOLE_OUTER}"); got != "$HOLE_INNER" {
		t.Errorf("expanded value was rescanned: got %q", got)
	}
}

func TestStripTrailingSlashes(t *testing.T) {
	tests := map[string]string{
		"/a/b/":   "/a/b",
		"/a/b///": "/a/b",
		"/":       "/",
		"a/":      "a",
		"":        "",
	}
	for input, want := range tests {
		if got := StripTrailingSlashes(input); got != want {
			t.Errorf("StripTrailingSlashes(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveHostPath(t *testing.T) {
	t.Setenv("HOLE_TEST_DIR", "/opt/data")
	host := Host{Username: "tester", Home: "/home/tester"}
	base := "/projects/demo"

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"absolute", "/etc/hosts", "/etc/hosts"},
		{"tilde", "~/.npmrc", "/home/tester/.npmrc"},
		{"bare tilde", "~", "/home/tester"},
		{"relative", "sub/dir", "/projects/demo/sub/dir"},
		{"env var", "${HOLE_TEST_DIR}/file", "/opt/data/file"},
		{"trailing slash", "/etc/dir/", "/etc/dir"},
		{"env var relative", "$HOLE_TEST_DIR", "/opt/data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := host.ResolveHostPath(test.raw, base); got != test.want {
				t.Errorf("ResolveHostPath(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestResolveContainerPath(t *testing.T) {
	host := Host{Home: "/home/tester"}
	projectDir := "/projects/demo"

	if got := host.ResolveContainerPath("~/.config", projectDir); got != "/home/tester/.config" {
		t.Errorf("tilde container path = %q", got)
	}
	if got := host.ResolveContainerPath("libs/dep", projectDir); got != "/projects/demo/libs/dep" {
		t.Errorf("relative container path = %q", got)
	}
	if got := host.ResolveContainerPath("/opt/libs", projectDir); got != "/opt/libs" {
		t.Errorf("absolute container path = %q", got)
	}
}

// The project directory is resolved exactly once, here, so everything downstream — the project
// name hash, the mounts, the instance registry — sees one spelling of the path. Symlinks are
// resolved because git prints physical paths, which is what internal/worktree has to match its
// output against.
func TestResolveProjectDirIsAbsoluteAndSymlinkFree(t *testing.T) {
	dir := t.TempDir()
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, target := range []string{dir, physical, link, dir + string(filepath.Separator)} {
		got, err := ResolveProjectDir(target)
		if err != nil {
			t.Fatalf("ResolveProjectDir(%q): %v", target, err)
		}
		if got != physical {
			t.Errorf("ResolveProjectDir(%q) = %q, want %q", target, got, physical)
		}
	}

	t.Chdir(dir)
	got, err := ResolveProjectDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if got != physical {
		t.Errorf(`ResolveProjectDir(".") = %q, want %q`, got, physical)
	}
}

func TestResolveProjectDirRejectsWhatCannotBeAProject(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The error names the target as the user typed it, not the absolute form, so it matches
	// what they can see on their command line.
	for _, testCase := range []struct {
		name    string
		target  string
		message string
	}{
		{"missing directory", filepath.Join(dir, "nope"), "does not exist"},
		{"a file", file, "is not a directory"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved, err := ResolveProjectDir(testCase.target)
			if err == nil {
				t.Fatalf("resolved %q to %q, want an error", testCase.target, resolved)
			}
			if !strings.Contains(err.Error(), testCase.message) {
				t.Errorf("error = %v, want it to mention %q", err, testCase.message)
			}
			if !strings.Contains(err.Error(), testCase.target) {
				t.Errorf("error = %v, want it to name the target %q", err, testCase.target)
			}
		})
	}
}

func TestProjectNameIsStableAndPathQualified(t *testing.T) {
	first := ProjectName("/Users/dev/Work/MyProject")
	again := ProjectName("/Users/dev/Work/MyProject")
	if first != again {
		t.Fatalf("project name is not stable: %q vs %q", first, again)
	}
	if want := "myproject-"; first[:len(want)] != want {
		t.Errorf("project name %q does not start with the sanitized basename", first)
	}
	if len(first) != len("myproject-")+8 {
		t.Errorf("project name %q does not end in an 8-character path hash", first)
	}

	// Same basename, different path — the hash suffix must keep them apart.
	other := ProjectName("/Users/dev/Other/MyProject")
	if first == other {
		t.Errorf("projects with the same basename collided: %q", first)
	}
}

// Paths that sanitize to the same string must still get distinct names: the name is the resource
// prefix `hole destroy` force-removes by, so a collision destroys a live sandbox of another
// project.
func TestProjectNameDistinguishesPathsThatSanitizeAlike(t *testing.T) {
	paths := []string{
		"/home/me/my_project",
		"/home/me/myproject",
		"/home/me/MyProject",
		"/home/me/my.project",
	}
	seen := map[string]string{}
	for _, path := range paths {
		name := ProjectName(path)
		if previous, clash := seen[name]; clash {
			t.Errorf("%q and %q both produce project name %q", previous, path, name)
			continue
		}
		seen[name] = path
	}
}

func TestProjectNameSanitizesInvalidCharacters(t *testing.T) {
	name := ProjectName("/tmp/My Project (v2)!")
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			t.Fatalf("project name %q contains invalid character %q", name, r)
		}
	}
}

func TestInstanceID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := InstanceID()
		if err != nil {
			t.Fatalf("InstanceID: %v", err)
		}
		if len(id) != instanceIDLength {
			t.Fatalf("instance id %q has length %d, want %d", id, len(id), instanceIDLength)
		}
		for _, r := range id {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				t.Fatalf("instance id %q contains invalid character %q", id, r)
			}
		}
		seen[id] = true
	}
	if len(seen) < 150 {
		t.Errorf("instance ids are not random enough: %d unique out of 200", len(seen))
	}
}

func TestHostBuildIDsDefaultOutsideLinux(t *testing.T) {
	host := Host{}
	if host.BuildUID() != "1000" || host.BuildGID() != "1000" {
		t.Errorf("expected image defaults when host IDs are unset, got %s/%s", host.BuildUID(), host.BuildGID())
	}
	host = Host{UID: "501", GID: "20"}
	if host.BuildUID() != "501" || host.BuildGID() != "20" {
		t.Errorf("host IDs not passed through: %s/%s", host.BuildUID(), host.BuildGID())
	}
}

func TestHoleDirectories(t *testing.T) {
	host := Host{Home: "/home/tester"}
	if got := host.HoleDir(); got != filepath.Join("/home/tester", ".hole") {
		t.Errorf("HoleDir = %q", got)
	}
	if got := host.TmpRoot(); got != "/home/tester/.hole/tmp" {
		t.Errorf("TmpRoot = %q — must live under $HOME for Colima/Lima bind mounts", got)
	}
	if got := host.UserAgentsDir(); got != "/home/tester/.hole/agents" {
		t.Errorf("UserAgentsDir = %q", got)
	}
}
