// Package agents is the agent plugin registry: builtin agents come from the embedded
// asset tree, user agents from ~/.hole/agents/<name>/. Both use the same file contract,
// so a user agent is a first-class agent everywhere (including allow-list merging and
// image builds).
package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/lukashornych/hole/assets"
)

// NamePattern is the allowed agent name shape. It excludes `:` because the CLI splits the
// agent positional on the first colon to select a profile.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

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
	allowFile       = "allow.txt"
	installRootFile = "install-root.sh"
	installUserFile = "install-user.sh"
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
func (r *Registry) ResolveEnabled(configured []string) ([]*Agent, error) {
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
