// Package config loads, validates and merges Hole's settings documents and exposes them
// as a typed model. It owns the merge semantics (objects deep-merge project-wins, arrays
// concatenate and deduplicate, scalars project-wins) and the exclusion glob matcher.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// ScriptEntry is one hook script reference.
type ScriptEntry struct {
	Script string `json:"script"`
}

// Library is a directory mounted next to the project, read-only unless ReadWrite is set.
type Library struct {
	Path      string `json:"path"`
	ReadWrite bool   `json:"readwrite"`
}

// UnmarshalJSON accepts both the string shorthand (read-only) and the object form.
func (l *Library) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		l.Path = asString
		l.ReadWrite = false
		return nil
	}
	var asObject struct {
		Path      string `json:"path"`
		ReadWrite bool   `json:"readwrite"`
	}
	if err := json.Unmarshal(data, &asObject); err != nil {
		return fmt.Errorf("library must be a string or an object with a path: %w", err)
	}
	l.Path = asObject.Path
	l.ReadWrite = asObject.ReadWrite
	return nil
}

// FilesSettings controls what the sandbox can see of the host filesystem.
type FilesSettings struct {
	Exclude []string          `json:"exclude"`
	Include map[string]string `json:"include"`
}

// NetworkSettings controls sandbox egress.
type NetworkSettings struct {
	// Allow lists egress rules as `<host>[:<port>[,<port>...]]`. Ports apply to TCP and UDP
	// alike and default to 443 and 80.
	Allow []string `json:"allow"`
	// HostGatewayDomains resolve to the Docker host gateway, optionally restricted to ports.
	HostGatewayDomains []string `json:"hostGatewayDomains"`
	SubnetPool         string   `json:"subnetPool"`
	// BridgeNetfilterFix controls the DOCKER-USER accept rule that restores same-bridge
	// traffic on hosts where br_netfilter filters bridged packets: "auto" (default, empty
	// means auto) installs it when the host needs it, "off" leaves the host firewall alone.
	BridgeNetfilterFix string `json:"bridgeNetfilterFix"`
}

// ContainerSettings controls the sandbox container itself.
type ContainerSettings struct {
	MemoryLimit     string   `json:"memoryLimit"`
	MemorySwapLimit string   `json:"memorySwapLimit"`
	Docker          bool     `json:"docker"`
	BaseImage       string   `json:"baseImage"`
	EnabledAgents   []string `json:"enabledAgents"`
}

// HookSettings holds the lifecycle hook scripts. Every entry may be a literal path or a
// glob; matches run in lexicographic order, so `NNN-` prefixes give a stable sequence.
type HookSettings struct {
	Setup       []ScriptEntry `json:"setup"`
	Prestart    []ScriptEntry `json:"prestart"`
	SetupHost   []ScriptEntry `json:"setupHost"`
	CleanupHost []ScriptEntry `json:"cleanupHost"`
}

// GitSettings controls the git-derived libraries.
type GitSettings struct {
	// WorktreeLinks is "ro" (default), "rw" or "off".
	WorktreeLinks string `json:"worktreeLinks"`
	// WorktreePool mounts a read-write `<project>-worktrees` sibling directory the agent can
	// create worktrees in. Only in the main repository, and only when WorktreeLinks is not off.
	WorktreePool bool `json:"worktreePool"`
}

// AgentSettings holds per-agent defaults.
type AgentSettings struct {
	// Args are prepended to the arguments given after `--`, so an ad-hoc flag has the final
	// say on any value flag they set.
	Args []string `json:"args"`
}

// Settings is the merged, typed settings document.
type Settings struct {
	Files        FilesSettings            `json:"files"`
	Network      NetworkSettings          `json:"network"`
	Dependencies []string                 `json:"dependencies"`
	Container    ContainerSettings        `json:"container"`
	Hooks        HookSettings             `json:"hooks"`
	Libraries    map[string]Library       `json:"libraries"`
	Environment  map[string]string        `json:"environment"`
	Agents       map[string]AgentSettings `json:"agents"`
	Git          GitSettings              `json:"git"`
}

// AgentArgs returns the default arguments configured for one agent.
func (s *Settings) AgentArgs(agent string) []string {
	if s.Agents == nil {
		return nil
	}
	return s.Agents[agent].Args
}

// Document is a raw settings document as read from disk, kept untyped so merging can stay
// generic and so unknown-but-valid future keys survive a round trip.
type Document map[string]any

// Load reads a settings document. A missing file is not an error — both settings files are
// optional — and yields a nil document.
func Load(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return doc, nil
}

// LoadAndValidate reads one settings file, reports removed 2.0 keys with a migration hint,
// then validates it against the schema. A missing file yields a nil document — both settings
// files are optional.
//
// The migration check runs first on purpose: a removed key would otherwise surface as a bare
// "additional properties not allowed", which tells the user nothing about what replaced it.
func LoadAndValidate(path, label string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var document Document
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, &ValidationFailure{Label: label, Details: []string{fmt.Sprintf("not valid JSON: %v", err)}}
	}
	if err := CheckRemovedKeys(label, document); err != nil {
		return nil, err
	}
	if err := ValidateBytes(data, label); err != nil {
		return nil, err
	}
	return document, nil
}

// Decode converts a merged document into the typed model.
func Decode(doc Document) (*Settings, error) {
	settings := &Settings{}
	if doc == nil {
		return settings, nil
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode merged settings: %w", err)
	}
	if err := json.Unmarshal(data, settings); err != nil {
		return nil, fmt.Errorf("decode merged settings: %w", err)
	}
	return settings, nil
}

// SortedKeys returns map keys in a stable order. Every generated artifact iterates maps
// through this so compose files and image hashes are reproducible.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
