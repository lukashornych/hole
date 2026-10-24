package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const schemaLine = `  "$schema": "` + schemaResourceURL + `"`

func TestSettingsTemplateIsValid(t *testing.T) {
	template := SettingsTemplate()
	if err := ValidateBytes(template, "template"); err != nil {
		t.Fatalf("template is not valid: %v", err)
	}
	if !strings.Contains(string(template), schemaResourceURL) {
		t.Errorf("template %q does not reference %s", template, schemaResourceURL)
	}
}

func TestWriteSettingsTemplateCreatesMissingFile(t *testing.T) {
	modes := map[string]TemplateMode{"create": TemplateCreate, "overwrite": TemplateOverwrite, "append": TemplateAppend}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".hole", "settings.json")
			existed, err := WriteSettingsTemplate(path, mode)
			if err != nil {
				t.Fatalf("WriteSettingsTemplate() error = %v", err)
			}
			if existed {
				t.Error("existed = true for a missing file")
			}
			assertContent(t, path, string(SettingsTemplate()))
		})
	}
}

func TestWriteSettingsTemplateExistingFile(t *testing.T) {
	tests := map[string]struct {
		mode     TemplateMode
		existing string
		want     string
		wantErr  bool
	}{
		"create refuses": {
			mode:     TemplateCreate,
			existing: `{"files":{}}`,
			want:     `{"files":{}}`,
			wantErr:  true,
		},
		"overwrite replaces": {
			mode:     TemplateOverwrite,
			existing: `{"files":{}}`,
			want:     string(SettingsTemplate()),
		},
		"append without schema puts it first": {
			mode:     TemplateAppend,
			existing: `{"network":{"allow":["b","a"]},"files":{"exclude":[".env"]}}`,
			want: "{\n" + schemaLine + ",\n" +
				"  \"network\": {\n    \"allow\": [\n      \"b\",\n      \"a\"\n    ]\n  },\n" +
				"  \"files\": {\n    \"exclude\": [\n      \".env\"\n    ]\n  }\n}\n",
		},
		"append replaces a different schema in place": {
			mode:     TemplateAppend,
			existing: `{"files":{},"$schema":"https://example.com/old.json"}`,
			want:     "{\n  \"files\": {},\n" + schemaLine + "\n}\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeTestFile(t, path, test.existing, 0o644)
			existed, err := WriteSettingsTemplate(path, test.mode)
			if (err != nil) != test.wantErr {
				t.Fatalf("WriteSettingsTemplate() error = %v, wantErr %v", err, test.wantErr)
			}
			if !existed {
				t.Error("existed = false for an existing file")
			}
			if test.wantErr {
				if !errors.Is(err, ErrSettingsExist) {
					t.Errorf("error %v is not ErrSettingsExist", err)
				}
				for _, want := range []string{path, "--overwrite", "--append"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %s", err, want)
					}
				}
			}
			assertContent(t, path, test.want)
		})
	}
}

func TestWriteSettingsTemplateOverwriteKeepsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeTestFile(t, path, `{}`, 0o600)
	if _, err := WriteSettingsTemplate(path, TemplateOverwrite); err != nil {
		t.Fatalf("WriteSettingsTemplate() error = %v", err)
	}
	assertContent(t, path, string(SettingsTemplate()))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteSettingsTemplateWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "settings.json")
	writeTestFile(t, target, `{}`, 0o644)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSettingsTemplate(link, TemplateOverwrite); err != nil {
		t.Fatalf("WriteSettingsTemplate() error = %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	assertContent(t, target, string(SettingsTemplate()))
}

func TestWriteSettingsTemplateRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteSettingsTemplate(dir, TemplateOverwrite); err == nil {
		t.Error("WriteSettingsTemplate() on a directory succeeded")
	}
}

func TestAppendTemplate(t *testing.T) {
	tests := map[string]struct {
		existing string
		want     string
	}{
		"empty":           {existing: "", want: string(SettingsTemplate())},
		"whitespace only": {existing: "  \n", want: string(SettingsTemplate())},
		"empty object":    {existing: "{}", want: string(SettingsTemplate())},
		"nested order kept": {
			existing: `{"profiles":{"z":{},"a":{"extends":["z"]}}}`,
			want: "{\n" + schemaLine + ",\n  \"profiles\": {\n    \"z\": {},\n    \"a\": {\n" +
				"      \"extends\": [\n        \"z\"\n      ]\n    }\n  }\n}\n",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := AppendTemplate([]byte(test.existing), SettingsTemplate())
			if err != nil {
				t.Fatalf("AppendTemplate() error = %v", err)
			}
			if string(got) != test.want {
				t.Errorf("AppendTemplate() =\n%s\nwant\n%s", got, test.want)
			}
		})
	}
}

func TestAppendTemplateRejectsUnmergeableInput(t *testing.T) {
	for _, existing := range []string{`[]`, `x`, `{`, `{} {}`, `{"a":1,"a":2}`} {
		t.Run(existing, func(t *testing.T) {
			_, err := AppendTemplate([]byte(existing), SettingsTemplate())
			if err == nil {
				t.Fatal("AppendTemplate() succeeded")
			}
			if !strings.Contains(err.Error(), "--overwrite") {
				t.Errorf("error %q does not mention --overwrite", err)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s =\n%s\nwant\n%s", path, got, want)
	}
}
