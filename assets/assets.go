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
	"path"
	"sort"
	"strings"
)

//go:embed agents gateway schema
var FS embed.FS

// Rootless assets have a separate hash: opting out retains the existing image inputs.
//
//go:embed rootless
var Rootless embed.FS

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

// UpdateOnlyFile reports whether a file of an agent directory (a path relative to it) is used
// only by in-place agent updates. Such files are not part of the image, and the update build reads
// the current ones, so they stay out of the image hashes: changing one must not rebuild images.
func UpdateOnlyFile(name string) bool {
	switch name {
	case "update-root.sh", "update-user.sh", "version.json":
		return true
	}
	return false
}

// BuildInputsHash is a stable digest of every embedded asset except the update-only files. It
// participates in the agent image tag so a Hole upgrade that changes a Dockerfile, entrypoint or
// install script invalidates cached images without the user asking for a rebuild.
func BuildInputsHash() string {
	return buildInputsHash(FS)
}

func buildInputsHash(fsys fs.FS) string {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !updateOnlyAsset(name) {
			paths = append(paths, name)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	sort.Strings(paths)

	h := sha1.New()
	for _, p := range paths {
		data, err := fs.ReadFile(fsys, p)
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

// updateOnlyAsset reports whether an embedded path is the update Dockerfile or an update-only file
// of a builtin agent.
func updateOnlyAsset(name string) bool {
	if name == "agents/Dockerfile.update" {
		return true
	}
	agentFile, ok := strings.CutPrefix(name, "agents/")
	return ok && path.Dir(agentFile) != "." && UpdateOnlyFile(strings.SplitN(agentFile, "/", 2)[1])
}
