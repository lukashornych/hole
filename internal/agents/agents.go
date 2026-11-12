// Package agents is the agent plugin registry: builtin agents come from the embedded
// asset tree, user agents from ~/.hole/agents/<name>/. Both use the same file contract,
// so a user agent is a first-class agent everywhere (including allow-list merging and
// image builds).
package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lukashornych/hole/v2/assets"
)

// NamePattern is the allowed agent name shape. It excludes `:` because the CLI splits the
// agent positional on the first colon to select a profile.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// npmPackagePattern is the shape of an npm package name, optionally scoped. It keeps a
// version.json from smuggling a path or query into the registry URL.
var npmPackagePattern = regexp.MustCompile(`^(@[a-z0-9~-][a-z0-9._~-]*/)?[a-z0-9~-][a-z0-9._~-]*$`)

// Source distinguishes builtin agents from user-provided ones.
type Source string

const (
	// SourceBuiltin marks an agent shipped inside the Hole binary.
	SourceBuiltin Source = "builtin"
	// SourceUser marks an agent discovered under ~/.hole/agents.
	SourceUser Source = "user"
)

const (
	commandFile     = "command.json"
	bareCommandFile = "command-bare.json"
	allowFile       = "allow.txt"
	installRootFile = "install-root.sh"
	installUserFile = "install-user.sh"
	updateRootFile  = "update-root.sh"
	updateUserFile  = "update-user.sh"
	versionFile     = "version.json"
)

// Agent is one agent plugin.
type Agent struct {
	Name   string
	Source Source
	// Dir serves the plugin's files; Path is only set for user agents and is used for
	// error messages and image-hash inputs.
	Dir  fs.FS
	Path string
}

// Command reads the agent's startup command (argv parts).
func (a *Agent) Command() ([]string, error) {
	data, err := fs.ReadFile(a.Dir, commandFile)
	if err != nil {
		return nil, fmt.Errorf("agent '%s': %s is missing or unreadable: %w", a.Name, commandFile, err)
	}
	var parts []string
	if err := json.Unmarshal(data, &parts); err != nil {
		return nil, fmt.Errorf("agent '%s': %s must be a JSON array of strings: %w", a.Name, commandFile, err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("agent '%s': %s must not be empty", a.Name, commandFile)
	}
	return parts, nil
}

// BareCommand reads the agent's optional command-bare.json: the argv a wrapper named after the
// agent runs, so the CLI is callable by bare name inside the sandbox. An agent without the file
// yields nil.
func (a *Agent) BareCommand() ([]string, error) {
	data, err := fs.ReadFile(a.Dir, bareCommandFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent '%s': read %s: %w", a.Name, bareCommandFile, err)
	}
	var parts []string
	if err := json.Unmarshal(data, &parts); err != nil {
		return nil, fmt.Errorf("agent '%s': %s must be a JSON array of strings: %w", a.Name, bareCommandFile, err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("agent '%s': %s must not be empty", a.Name, bareCommandFile)
	}
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("agent '%s': %s must not contain an empty item", a.Name, bareCommandFile)
		}
	}
	// A bare first part is looked up on PATH, which the wrapper itself leads: ["my-agent"] would
	// exec itself forever.
	if !strings.Contains(parts[0], "/") {
		return nil, fmt.Errorf("agent '%s': %s must start with a path to the executable, not the bare name '%s'",
			a.Name, bareCommandFile, parts[0])
	}
	return parts, nil
}

// AllowFile returns the raw content of the agent's built-in allow list.
func (a *Agent) AllowFile() ([]byte, error) {
	data, err := fs.ReadFile(a.Dir, allowFile)
	if err != nil {
		return nil, fmt.Errorf("agent '%s': %s is missing or unreadable: %w", a.Name, allowFile, err)
	}
	return data, nil
}

// InstallScripts returns the agent's optional install scripts by file name.
func (a *Agent) InstallScripts() (map[string][]byte, error) {
	scripts := map[string][]byte{}
	for _, name := range []string{installRootFile, installUserFile} {
		data, err := fs.ReadFile(a.Dir, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("agent '%s': read %s: %w", a.Name, name, err)
		}
		scripts[name] = data
	}
	return scripts, nil
}

// UpdateScripts returns the agent's optional update scripts by file name. An agent without any
// cannot be updated in place and is left to a clean rebuild.
func (a *Agent) UpdateScripts() (map[string][]byte, error) {
	scripts := map[string][]byte{}
	for _, name := range []string{updateRootFile, updateUserFile} {
		data, err := fs.ReadFile(a.Dir, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("agent '%s': read %s: %w", a.Name, name, err)
		}
		scripts[name] = data
	}
	return scripts, nil
}

// VersionSpec says how to learn an agent's installed and latest version.
type VersionSpec struct {
	// Command prints the installed version when run inside the sandbox image; `$HOME` refers to
	// the sandbox home, as in command.json.
	Command []string `json:"command"`
	// Latest is where Hole looks up the newest release, over plain HTTP from the host.
	Latest LatestSource `json:"latest"`
}

// LatestSource is exactly one place the newest version of an agent is published.
type LatestSource struct {
	// NPM is a package name whose `latest` dist-tag is read from the npm registry.
	NPM string `json:"npm,omitempty"`
	// URL serves the version as plain text or as a `{"version": "…"}` document.
	URL string `json:"url,omitempty"`
}

// VersionSpec reads the agent's optional version.json; an agent without one yields nil.
func (a *Agent) VersionSpec() (*VersionSpec, error) {
	data, err := fs.ReadFile(a.Dir, versionFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent '%s': read %s: %w", a.Name, versionFile, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var spec VersionSpec
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("agent '%s': %s is not valid: %w", a.Name, versionFile, err)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return nil, fmt.Errorf("agent '%s': %s needs a non-empty command", a.Name, versionFile)
	}
	if (spec.Latest.NPM == "") == (spec.Latest.URL == "") {
		return nil, fmt.Errorf("agent '%s': %s needs exactly one latest source, npm or url", a.Name, versionFile)
	}
	if spec.Latest.NPM != "" && !npmPackagePattern.MatchString(spec.Latest.NPM) {
		return nil, fmt.Errorf("agent '%s': %s latest.npm '%s' is not an npm package name",
			a.Name, versionFile, spec.Latest.NPM)
	}
	if spec.Latest.URL != "" {
		parsed, err := url.Parse(spec.Latest.URL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return nil, fmt.Errorf("agent '%s': %s latest.url must be an http(s) URL, got '%s'",
				a.Name, versionFile, spec.Latest.URL)
		}
	}
	return &spec, nil
}

// CheckUpdateFiles rejects a half-configured in-place update: an update script without a
// version.json would be updated but never verified, a version.json without an update script would
// offer updates nothing can install. An agent has both or neither.
func (a *Agent) CheckUpdateFiles() error {
	scripts, err := a.UpdateScripts()
	if err != nil {
		return err
	}
	spec, err := a.VersionSpec()
	if err != nil {
		return err
	}
	switch {
	case len(scripts) > 0 && spec == nil:
		return fmt.Errorf("agent '%s' has an update script but no %s; add %s or remove the update scripts%s",
			a.Name, versionFile, versionFile, a.location())
	case len(scripts) == 0 && spec != nil:
		return fmt.Errorf("agent '%s' has a %s but no update script; add %s or remove %s%s",
			a.Name, versionFile, updateUserFile, versionFile, a.location())
	}
	return nil
}

func (a *Agent) location() string {
	if a.Path == "" {
		return ""
	}
	return " (in " + a.Path + ")"
}

// Registry holds all discovered agents.
type Registry struct {
	byName map[string]*Agent
}

// Load builds the registry from the embedded builtins plus user agents in userAgentsDir.
// A user agent whose name collides with a builtin is fatal: silently shadowing `claude`
// would change what a well-known command starts.
func Load(userAgentsDir string) (*Registry, error) {
	registry := &Registry{byName: map[string]*Agent{}}

	builtinRoot := assets.Agents()
	entries, err := fs.ReadDir(builtinRoot, ".")
	if err != nil {
		return nil, fmt.Errorf("read builtin agents: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir, err := fs.Sub(builtinRoot, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read builtin agent '%s': %w", entry.Name(), err)
		}
		registry.byName[entry.Name()] = &Agent{Name: entry.Name(), Source: SourceBuiltin, Dir: dir}
	}

	if userAgentsDir == "" {
		return registry, nil
	}
	userEntries, err := os.ReadDir(userAgentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return registry, nil
		}
		return nil, fmt.Errorf("read user agents from %s: %w", userAgentsDir, err)
	}
	for _, entry := range userEntries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !NamePattern.MatchString(name) {
			return nil, fmt.Errorf("user agent directory '%s' is not a valid agent name (expected %s)",
				filepath.Join(userAgentsDir, name), NamePattern.String())
		}
		if existing, ok := registry.byName[name]; ok && existing.Source == SourceBuiltin {
			return nil, fmt.Errorf("user agent '%s' collides with the builtin agent of the same name; rename %s",
				name, filepath.Join(userAgentsDir, name))
		}
		path := filepath.Join(userAgentsDir, name)
		registry.byName[name] = &Agent{
			Name:   name,
			Source: SourceUser,
			Dir:    os.DirFS(path),
			Path:   path,
		}
	}
	return registry, nil
}

// Get looks up an agent by name.
func (r *Registry) Get(name string) (*Agent, bool) {
	agent, ok := r.byName[name]
	return agent, ok
}

// Names lists all registered agent names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve validates an agent name against the registry.
func (r *Registry) Resolve(name string) (*Agent, error) {
	if name == "" {
		return nil, fmt.Errorf("no agent given")
	}
	if !NamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid agent '%s' (expected %s)", name, NamePattern.String())
	}
	agent, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("invalid agent '%s'; available agents: %v", name, r.Names())
	}
	return agent, nil
}

// ResolveEnabled returns the agents whose CLIs are installed into the sandbox image. An
// absent or empty configured list means every registered agent, matching the bash default.
// An enabled agent with inconsistent update files is an error; a disabled one is not checked.
func (r *Registry) ResolveEnabled(configured []string) ([]*Agent, error) {
	enabled, err := r.ResolveEnabledUnchecked(configured)
	if err != nil {
		return nil, err
	}
	for _, agent := range enabled {
		if err := agent.CheckUpdateFiles(); err != nil {
			return nil, err
		}
	}
	return enabled, nil
}

// ResolveEnabledUnchecked is ResolveEnabled without the update-file check, for a configuration
// that only describes the image scope and is not itself started.
func (r *Registry) ResolveEnabledUnchecked(configured []string) ([]*Agent, error) {
	if len(configured) == 0 {
		enabled := make([]*Agent, 0, len(r.byName))
		for _, name := range r.Names() {
			enabled = append(enabled, r.byName[name])
		}
		return enabled, nil
	}

	seen := map[string]bool{}
	enabled := make([]*Agent, 0, len(configured))
	for _, name := range configured {
		if seen[name] {
			continue
		}
		agent, ok := r.byName[name]
		if !ok {
			return nil, fmt.Errorf("container.enabledAgents lists unknown agent '%s'; available agents: %v", name, r.Names())
		}
		seen[name] = true
		enabled = append(enabled, agent)
	}
	return enabled, nil
}

// EnabledNames is the name list of a resolved enabled-agent set, in resolution order.
func EnabledNames(enabled []*Agent) []string {
	names := make([]string, 0, len(enabled))
	for _, agent := range enabled {
		names = append(names, agent.Name)
	}
	return names
}
