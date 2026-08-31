// Package assets holds every runtime file Hole ships: agent plugins, the sandbox and
// gateway Dockerfiles, container entrypoints and the settings JSON Schema.
//
// Runtime assets must live under this directory and be covered by an embed directive
// below — a missing file then fails the build instead of silently disappearing from a
// release artifact.
package assets

import (
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
)

//go:embed agents gateway schema
var FS embed.FS

// Agents is the embedded builtin agent plugin tree (one directory per agent).
func Agents() fs.FS {
	sub, err := fs.Sub(FS, "agents")
	if err != nil {
		panic(err) // embed directives are compile-time constants
	}
	return sub
}

// Gateway is the embedded gateway image build context.
func Gateway() fs.FS {
	sub, err := fs.Sub(FS, "gateway")
	if err != nil {
		panic(err)
	}
	return sub
}

// Schema returns the embedded settings JSON Schema document.
func Schema() []byte {
	data, err := FS.ReadFile("schema/settings.schema.json")
	if err != nil {
		panic(err)
	}
	return data
}

// BuildInputsHash is a stable digest of every embedded asset. It participates in the
// agent image tag so a Hole upgrade that changes a Dockerfile, entrypoint or install
// script invalidates cached images without the user asking for a rebuild.
func BuildInputsHash() string {
	var paths []string
	err := fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	sort.Strings(paths)

	h := sha1.New()
	for _, p := range paths {
		data, err := FS.ReadFile(p)
		if err != nil {
			panic(err)
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
