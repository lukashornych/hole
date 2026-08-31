// Package image derives sandbox image identity. Images are tagged with a hash of
// everything that affects their content, so changing an image-affecting setting produces a
// new tag — which compose then builds automatically, with no `-r` needed.
package image

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DefaultBaseImage is the image built on when container.baseImage is unset.
const DefaultBaseImage = "ubuntu:24.04"

// GatewayRepository holds the gateway image, which every sandbox shares: its configuration
// files are runtime mounts, so no user setting changes its content.
const GatewayRepository = "hole-sandbox/gateway"

// AgentRepositoryPrefix precedes the project name in agent image repositories.
const AgentRepositoryPrefix = "hole-sandbox/agent-"

// GlobalRepository holds the image shared by every project whose configuration does not
// change the image. A project name always ends in an 8-hex path hash, so no project can
// collide with the literal `global` here.
const GlobalRepository = AgentRepositoryPrefix + "global"

// ImageLabel marks Hole's own images so a dangling-image prune can be restricted to them and
// never touches a user's unrelated leftovers.
const ImageLabel = "com.hole.image"

// tagLength is how much of the manifest hash ends up in the tag.
const tagLength = 12

// Config is the canonical image configuration: every setting that changes image content,
// normalized so "explicitly set to the default" and "not set" are indistinguishable.
type Config struct {
	BaseImage     string   `json:"baseImage"`
	EnabledAgents []string `json:"enabledAgents"`
	// Dependencies keep post-merge order — it is exactly the EXTRA_PACKAGES order.
	Dependencies []string `json:"dependencies"`
	// SetupScriptShas hashes script *content* in run order, not paths: a project pointing
	// at an identical copy of a global script legitimately shares its image, while a
	// project overriding it with different content legitimately does not.
	SetupScriptShas []string `json:"setupScriptShas"`
}

// Normalize applies defaults so equal configurations compare equal.
func (c Config) Normalize() Config {
	out := c
	if out.BaseImage == "" {
		out.BaseImage = DefaultBaseImage
	}
	if out.EnabledAgents == nil {
		out.EnabledAgents = []string{}
	}
	if out.Dependencies == nil {
		out.Dependencies = []string{}
	}
	if out.SetupScriptShas == nil {
		out.SetupScriptShas = []string{}
	}
	return out
}

// HostIdentity is the part of the host user that is baked into the image. Two host users
// sharing one daemon must not collide on a cached image.
type HostIdentity struct {
	Username string `json:"username"`
	Home     string `json:"home"`
	UID      string `json:"uid"`
	GID      string `json:"gid"`
}

// Manifest is everything the image tag hashes over. CACHEBUST is deliberately absent: it
// is a rebuild trigger, not configuration.
type Manifest struct {
	Config Config       `json:"config"`
	Host   HostIdentity `json:"host"`
	// BuildInputs is the digest of Hole's own embedded assets (Dockerfile, entrypoint,
	// builtin agent install scripts).
	BuildInputs string `json:"buildInputs"`
	// UserAgents are `<name>:<sha>` pairs for enabled user agents, so editing a user
	// agent's install script invalidates the image.
	UserAgents []string `json:"userAgents"`
}

// Identity is a resolved image reference.
type Identity struct {
	Repository string
	Tag        string
	Scope      Scope
	// DifferingKeys explains a project scope: the canonical configuration keys whose values
	// differ from the global-only configuration.
	DifferingKeys []string
}

// Describe renders the image with its scope, for the start banner.
func (i Identity) Describe() string {
	if i.Scope == ScopeGlobal {
		return i.Reference() + " (shared)"
	}
	if len(i.DifferingKeys) == 0 {
		return i.Reference() + " (project-specific)"
	}
	return i.Reference() + " (project-specific: " + strings.Join(i.DifferingKeys, ", ") + ")"
}

// Reference is the full `repository:tag` string.
func (i Identity) Reference() string { return i.Repository + ":" + i.Tag }

// AgentRepository is the image repository for one project's sandbox image.
func AgentRepository(projectName string) string {
	return AgentRepositoryPrefix + projectName
}

// GatewayImage is the gateway image reference for a given embedded-assets digest.
//
// The tag is content-derived for the same reason the agent tag is: the gateway's Dockerfile and
// entrypoint ship inside the binary, so a Hole release can change them, and compose never
// rebuilds a tag that already exists. A fixed tag would leave every existing installation
// running the gateway it first built — a stale firewall and DNS policy engine that no upgrade
// could replace.
func GatewayImage(buildInputsHash string) string {
	tag := buildInputsHash
	if len(tag) > tagLength {
		tag = tag[:tagLength]
	}
	return GatewayRepository + ":" + tag
}

// Scope records whether an image is shared between projects or specific to one.
type Scope string

const (
	// ScopeGlobal means the project does not change the image, so it uses the shared one.
	ScopeGlobal Scope = "global"
	// ScopeProject means the project modifies the image and gets its own repository.
	ScopeProject Scope = "project"
)

// Resolve decides which image a run uses.
//
// The comparison is between two *canonicalised* configurations, not "does the project file
// mention image keys": a project that repeats a global value verbatim, or adds a dependency
// that deduplicates away, legitimately keeps the shared image. Only a project that actually
// changes image content gets its own repository.
//
// DifferingKeys names what made it project-specific, so the start banner can say why.
func Resolve(projectName string, merged, globalOnly Manifest) (Identity, error) {
	tag, err := merged.Tag()
	if err != nil {
		return Identity{}, err
	}

	differing := merged.Config.Normalize().differingKeys(globalOnly.Config.Normalize())
	if len(differing) == 0 {
		return Identity{Repository: GlobalRepository, Tag: tag, Scope: ScopeGlobal}, nil
	}
	return Identity{
		Repository:    AgentRepository(projectName),
		Tag:           tag,
		Scope:         ScopeProject,
		DifferingKeys: differing,
	}, nil
}

// differingKeys lists the canonical configuration keys two configurations disagree on.
func (c Config) differingKeys(other Config) []string {
	var keys []string
	if c.BaseImage != other.BaseImage {
		keys = append(keys, "baseImage")
	}
	if !equalStrings(c.EnabledAgents, other.EnabledAgents) {
		keys = append(keys, "enabledAgents")
	}
	if !equalStrings(c.Dependencies, other.Dependencies) {
		keys = append(keys, "dependencies")
	}
	if !equalStrings(c.SetupScriptShas, other.SetupScriptShas) {
		keys = append(keys, "setupScripts")
	}
	return keys
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// Tag is the first 12 hex characters of the sha1 over the canonical manifest.
func (m Manifest) Tag() (string, error) {
	normalized := m
	normalized.Config = m.Config.Normalize()
	sorted := append([]string(nil), normalized.UserAgents...)
	sort.Strings(sorted)
	normalized.UserAgents = sorted
	if normalized.UserAgents == nil {
		normalized.UserAgents = []string{}
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("serialize image manifest: %w", err)
	}
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])[:tagLength], nil
}

// ContentSHA hashes file content for the setup-script part of the manifest.
func ContentSHA(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}
