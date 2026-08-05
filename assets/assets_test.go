package assets

import (
	"io/fs"
	"testing"
)

func TestEmbeddedTreeIsComplete(t *testing.T) {
	required := []string{
		"agents/Dockerfile",
		"agents/entrypoint.sh",
		"agents/claude/command.json",
		"agents/claude/allow.txt",
		"agents/gemini/command.json",
		"agents/gemini/allow.txt",
		"agents/codex/command.json",
		"agents/codex/allow.txt",
		"gateway/Dockerfile",
		"gateway/entrypoint.sh",
		"schema/settings.schema.json",
	}
	for _, path := range required {
		if _, err := fs.Stat(FS, path); err != nil {
			t.Errorf("required asset %s is not embedded: %v", path, err)
		}
	}
}

func TestBuildInputsHashIsStable(t *testing.T) {
	first := BuildInputsHash()
	for i := 0; i < 5; i++ {
		if BuildInputsHash() != first {
			t.Fatal("BuildInputsHash is not stable within a build")
		}
	}
	if len(first) != 40 {
		t.Errorf("BuildInputsHash = %q, want a sha1 hex digest", first)
	}
}

func TestSchemaIsReadable(t *testing.T) {
	if len(Schema()) == 0 {
		t.Error("embedded schema is empty")
	}
}
