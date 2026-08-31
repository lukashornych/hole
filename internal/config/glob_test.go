package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tree creates a fixture directory:
//
//	.env
//	secret.txt
//	notes.md
//	config/app.yaml
//	config/db/creds.yaml
//	deep/a/b/target.key
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		".env",
		"secret.txt",
		"notes.md",
		"config/app.yaml",
		"config/db/creds.yaml",
		"deep/a/b/target.key",
	}
	for _, file := range files {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHasGlobChars(t *testing.T) {
	for _, value := range []string{"*.txt", "a?b", "[abc]"} {
		if !HasGlobChars(value) {
			t.Errorf("HasGlobChars(%q) = false", value)
		}
	}
	for _, value := range []string{".env", "config/app.yaml", "a-b_c.d"} {
		if HasGlobChars(value) {
			t.Errorf("HasGlobChars(%q) = true", value)
		}
	}
}

func TestExpandGlob(t *testing.T) {
	root := tree(t)
	tests := []struct {
		name    string
		pattern string
		want    []string
	}{
		{"single star", "*.md", []string{"notes.md"}},
		{"star matches no dotfiles implicitly is not our rule", "*.txt", []string{"secret.txt"}},
		{"nested literal segment", "config/*.yaml", []string{"config/app.yaml"}},
		{"question mark", "note?.md", []string{"notes.md"}},
		{"character class", "[ns]otes.md", []string{"notes.md"}},
		{"globstar spans segments", "**/*.yaml", []string{"config/app.yaml", "config/db/creds.yaml"}},
		{"globstar deep", "deep/**/*.key", []string{"deep/a/b/target.key"}},
		{"globstar matches zero segments", "config/**/app.yaml", []string{"config/app.yaml"}},
		{"no matches", "*.nope", nil},
		{"literal path via glob machinery", "config/app.yaml", []string{"config/app.yaml"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ExpandGlob(root, test.pattern)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("ExpandGlob(%q) = %v, want %v", test.pattern, got, test.want)
			}
		})
	}
}

func TestExpandGlobMatchesDirectoriesToo(t *testing.T) {
	root := tree(t)
	got := ExpandGlob(root, "config/*")
	want := []string{"config/app.yaml", "config/db"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandGlob(config/*) = %v, want %v", got, want)
	}
}

func TestExpandGlobResultsAreSortedAndDeduplicated(t *testing.T) {
	root := tree(t)
	got := ExpandGlob(root, "**/*.yaml")
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("results not sorted/deduplicated: %v", got)
		}
	}
}

func TestExpandGlobIgnoresMissingRoot(t *testing.T) {
	if got := ExpandGlob(filepath.Join(t.TempDir(), "absent"), "*"); got != nil {
		t.Errorf("expected no matches for a missing directory, got %v", got)
	}
}
