package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ProfileNamePattern is the allowed profile name shape. It excludes `:` (the CLI splits the
// agent positional on it) and `,` (reserved for a future multi-profile syntax), and is
// directly usable in image naming with no transformation.
var ProfileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// profilesKey and extendsKey are metadata: both are stripped before merging, so the merged
// result never contains them and no downstream consumer has to know profiles exist.
const (
	profilesKey = "profiles"
	extendsKey  = "extends"
)

// ValidateProfileName checks a requested profile name. The CLI calls it before any settings
// file is read, so a typo fails clearly even in a project with no settings at all.
func ValidateProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("empty profile name; expected %s", ProfileNamePattern.String())
	}
	if !ProfileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid profile name '%s'; expected %s", name, ProfileNamePattern.String())
	}
	return nil
}

// profilesOf returns the `profiles` object of a document.
func profilesOf(document Document) map[string]any {
	if document == nil {
		return nil
	}
	profiles, _ := document[profilesKey].(map[string]any)
	return profiles
}

// baseOf returns a document without its `profiles` key.
func baseOf(document Document) Document {
	if document == nil {
		return nil
	}
	base := Document{}
	for key, value := range document {
		if key == profilesKey {
			continue
		}
		base[key] = value
	}
	return base
}

// StripProfiles returns a document without its `profiles` key, which is metadata: the merged
// result never contains it, so the no-profile case degenerates to the plain two-way merge.
func StripProfiles(document Document) Document { return baseOf(document) }

// overlayOf returns one profile's settings from a document, with `extends` stripped. A
// profile the document does not define yields nil, which merges as nothing.
func overlayOf(document Document, name string) Document {
	profile, ok := profilesOf(document)[name].(map[string]any)
	if !ok {
		return nil
	}
	overlay := Document{}
	for key, value := range profile {
		if key == extendsKey {
			continue
		}
		overlay[key] = value
	}
	return overlay
}

// ProfileNames lists the profiles a document defines, sorted.
func ProfileNames(document Document) []string {
	profiles := profilesOf(document)
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveChain expands a selected profile into the ordered list of profiles to apply:
// parents before children, in listed order, each name once.
//
// The `extends` view is combined across both documents, so a project profile can extend a
// globally-defined one and vice versa. Diamonds are harmless under additive merge because a
// visited profile is applied only once.
func ResolveChain(global, project Document, selected string) ([]string, error) {
	if err := ValidateProfileName(selected); err != nil {
		return nil, err
	}
	if !definedIn(global, selected) && !definedIn(project, selected) {
		return nil, &UnknownProfileError{Name: selected, Global: ProfileNames(global), Project: ProfileNames(project)}
	}

	visited := map[string]bool{}
	var chain []string
	var expand func(name string, path []string) error
	expand = func(name string, path []string) error {
		for i, ancestor := range path {
			if ancestor == name {
				return fmt.Errorf("profile inheritance cycle: %s", strings.Join(append(path[i:], name), " -> "))
			}
		}
		if visited[name] {
			return nil
		}
		if !definedIn(global, name) && !definedIn(project, name) {
			return &UnknownProfileError{
				Name:    name,
				Parent:  path[len(path)-1],
				Global:  ProfileNames(global),
				Project: ProfileNames(project),
			}
		}
		for _, parent := range effectiveExtends(global, project, name) {
			if err := expand(parent, append(path, name)); err != nil {
				return err
			}
		}
		if visited[name] {
			return nil
		}
		visited[name] = true
		chain = append(chain, name)
		return nil
	}
	if err := expand(selected, nil); err != nil {
		return nil, err
	}
	return chain, nil
}

// effectiveExtends merges the `extends` lists of one profile across both documents, global
// first, concatenated and deduplicated — the same semantics every other array follows.
func effectiveExtends(global, project Document, name string) []string {
	var parents []string
	seen := map[string]bool{}
	for _, document := range []Document{global, project} {
		profile, ok := profilesOf(document)[name].(map[string]any)
		if !ok {
			continue
		}
		for _, parent := range extendsList(profile[extendsKey]) {
			if seen[parent] {
				continue
			}
			seen[parent] = true
			parents = append(parents, parent)
		}
	}
	return parents
}

// extendsList accepts both the string and the array form.
func extendsList(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			if name, ok := item.(string); ok && name != "" {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

func definedIn(document Document, name string) bool {
	_, ok := profilesOf(document)[name].(map[string]any)
	return ok
}

// UnknownProfileError reports a profile that no settings file defines. A silently ignored
// profile would run the sandbox with the wrong permissions, so this is fatal.
type UnknownProfileError struct {
	Name string
	// Parent is set when the unknown profile was reached through `extends`.
	Parent  string
	Global  []string
	Project []string
}

func (e *UnknownProfileError) Error() string {
	var sb strings.Builder
	if e.Parent != "" {
		fmt.Fprintf(&sb, "profile '%s' extends unknown profile '%s'", e.Parent, e.Name)
	} else {
		fmt.Fprintf(&sb, "unknown profile '%s'", e.Name)
	}
	fmt.Fprintf(&sb, "\n  profiles in ~/.hole/settings.json: %s", formatNames(e.Global))
	fmt.Fprintf(&sb, "\n  profiles in .hole/settings.json:   %s", formatNames(e.Project))
	return sb.String()
}

func formatNames(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// MergeWithProfile merges the settings documents for a run.
//
// Application order is global base → global overlays in chain order → project base →
// project overlays in chain order. That keeps the invariant that anything in the project
// file overrides anything global, while the leaf profile stays the highest-precedence
// overlay within each file.
//
// Profiles are additive only: an overlay can add to the base but never narrow it. Replace
// and remove semantics were considered and rejected — working out effective permissions
// would mean mentally evaluating subtraction across four sources.
func MergeWithProfile(global, project Document, chain []string) Document {
	documents := []Document{baseOf(global)}
	for _, name := range chain {
		documents = append(documents, overlayOf(global, name))
	}
	documents = append(documents, baseOf(project))
	for _, name := range chain {
		documents = append(documents, overlayOf(project, name))
	}
	merged := Merge(documents...)

	// Argument vectors must not be deduplicated: `["--tool", "a", "--tool", "b"]` would lose
	// its second flag. Recompute them as a plain concatenation of every contributing source.
	applyAgentArgs(merged, documents)
	return merged
}

// applyAgentArgs replaces each `agents.<name>.args` in the merged document with the plain
// concatenation of that key across all sources, in application order.
func applyAgentArgs(merged Document, sources []Document) {
	agentsValue, ok := merged["agents"].(map[string]any)
	if !ok {
		return
	}
	for agentName, entry := range agentsValue {
		agentEntry, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if _, hasArgs := agentEntry["args"]; !hasArgs {
			continue
		}
		var args []any
		for _, source := range sources {
			args = append(args, agentArgsOf(source, agentName)...)
		}
		agentEntry["args"] = args
	}
}

func agentArgsOf(document Document, agentName string) []any {
	if document == nil {
		return nil
	}
	agentsValue, ok := document["agents"].(map[string]any)
	if !ok {
		return nil
	}
	entry, ok := agentsValue[agentName].(map[string]any)
	if !ok {
		return nil
	}
	args, _ := entry["args"].([]any)
	return args
}
