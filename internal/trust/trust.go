// Package trust gates the capabilities a project's own `.hole/settings.json` may grant.
//
// The global settings file is the user's own document and is trusted implicitly. A project
// file is repository content: pointing an agent at an untrusted repository is the workflow
// Hole exists for, and that file can run scripts on the host, mount host paths into the
// sandbox, switch on the privileged Docker-in-Docker sidecar and widen the egress policy.
// Those grants therefore need the user's confirmation once per project, recorded in
// `~/.hole/trust.json`.
//
// Everything a project file can ask for that stays *inside* the sandbox is deliberately not
// gated — the sandbox is the boundary, so in-container effects need no separate consent. The
// test is whether an effect *leaves* the sandbox, which is why the network keys are gated too:
// widened egress is a channel out of it, and the gateway enforcing a policy the repository wrote
// is not a boundary.
package trust

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// grant is one capability a project settings document asks for.
type grant struct {
	// Key is the settings path, as written in the file (`hooks.setupHost`).
	Key string
	// Effect is the one-line consequence shown in the prompt.
	Effect string
	// Values are the requested values exactly as written in the file. Raw, never resolved:
	// an expanded path embeds the host's home directory, which would make the digest
	// machine-specific, and a changed `$VAR` that redirects a mount would not change it.
	Values []string
}

// capability describes one gated setting and how to render what it asks for.
type capability struct {
	key    string
	effect string
	values func(*config.Settings) []string
}

// capabilities are the settings a project file may not enable on its own, most host-reaching
// first. Settings whose effect is confined to the sandbox are absent on purpose: `files.exclude`
// only removes access, and `environment`, `agents.*.args`, `container.baseImage`,
// `hooks.prestart` and `network.subnetPool` act inside the container, which is the boundary.
var capabilities = []capability{
	{
		key:    "hooks.setupHost",
		effect: "runs a script on your host before the sandbox is created",
		values: func(s *config.Settings) []string { return scriptPaths(s.Hooks.SetupHost) },
	},
	{
		key:    "hooks.cleanupHost",
		effect: "runs a script on your host when the sandbox is destroyed",
		values: func(s *config.Settings) []string { return scriptPaths(s.Hooks.CleanupHost) },
	},
	{
		key:    "files.include",
		effect: "mounts host paths into the sandbox",
		values: includeValues,
	},
	{
		key:    "libraries",
		effect: "mounts host directories into the sandbox",
		values: libraryValues,
	},
	{
		key:    "container.docker",
		effect: "adds the privileged Docker-in-Docker sidecar",
		values: dockerValues,
	},
	{
		key:    "network.hostGatewayDomains",
		effect: "lets the sandbox reach services on your host",
		values: func(s *config.Settings) []string { return s.Network.HostGatewayDomains },
	},
	{
		key:    "hooks.setup",
		effect: "runs a script during the image build, on your host's unfiltered network",
		values: func(s *config.Settings) []string { return scriptPaths(s.Hooks.Setup) },
	},
	{
		key:    "dependencies",
		effect: "installs packages into the sandbox image, over your host's network",
		values: func(s *config.Settings) []string { return s.Dependencies },
	},
	{
		key:    "network.allow",
		effect: "widens the sandbox's network allow-list",
		values: func(s *config.Settings) []string { return s.Network.Allow },
	},
}

// requestedGrants returns what a project settings document asks for, in capability order.
//
// Profiles are scanned alongside the base settings: the file as a whole is what gets trusted,
// so which profile a run selects neither hides a grant nor invalidates the recorded decision.
func requestedGrants(document config.Document) ([]grant, error) {
	if document == nil {
		return nil, nil
	}

	requested := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, scope := range scopes(document) {
		settings, err := config.Decode(scope)
		if err != nil {
			return nil, err
		}
		for _, gated := range capabilities {
			for _, value := range gated.values(settings) {
				if seen[gated.key] == nil {
					seen[gated.key] = map[string]bool{}
				}
				if seen[gated.key][value] {
					continue
				}
				seen[gated.key][value] = true
				requested[gated.key] = append(requested[gated.key], value)
			}
		}
	}

	var grants []grant
	for _, gated := range capabilities {
		values := requested[gated.key]
		if len(values) == 0 {
			continue
		}
		grants = append(grants, grant{Key: gated.key, Effect: gated.effect, Values: values})
	}
	return grants, nil
}

// scopes returns the documents to scan: the base settings followed by every profile, in name
// order so the grant list — and therefore the digest — does not depend on map iteration.
func scopes(document config.Document) []config.Document {
	documents := []config.Document{document}
	profiles, ok := document["profiles"].(map[string]any)
	if !ok {
		return documents
	}
	for _, name := range config.SortedKeys(profiles) {
		if profile, ok := profiles[name].(map[string]any); ok {
			documents = append(documents, config.Document(profile))
		}
	}
	return documents
}

// grantKeys lists the settings keys of a grant set, for one-line messages.
func grantKeys(grants []grant) []string {
	keys := make([]string, 0, len(grants))
	for _, granted := range grants {
		keys = append(keys, granted.Key)
	}
	return keys
}

// digestOf fingerprints a grant set. Trust is recorded against it, so widening what a project
// asks for prompts again while an ungated settings edit does not.
func digestOf(grants []grant) string {
	hash := sha256.New()
	for _, granted := range grants {
		fmt.Fprintf(hash, "%s\n", granted.Key)
		for _, value := range granted.Values {
			fmt.Fprintf(hash, "\t%s\n", value)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func scriptPaths(entries []config.ScriptEntry) []string {
	var paths []string
	for _, entry := range entries {
		if entry.Script != "" {
			paths = append(paths, entry.Script)
		}
	}
	return paths
}

func includeValues(settings *config.Settings) []string {
	var values []string
	for _, hostPath := range config.SortedKeys(settings.Files.Include) {
		values = append(values, hostPath+" -> "+settings.Files.Include[hostPath])
	}
	return values
}

func libraryValues(settings *config.Settings) []string {
	var values []string
	for _, hostPath := range config.SortedKeys(settings.Libraries) {
		library := settings.Libraries[hostPath]
		value := hostPath + " -> " + library.Path
		if library.ReadWrite {
			value += " (read-write)"
		}
		values = append(values, value)
	}
	return values
}

func dockerValues(settings *config.Settings) []string {
	if !settings.Container.Docker {
		return nil
	}
	return []string{"true"}
}

// Options are the inputs to Gate.
type Options struct {
	// ProjectDir is the resolved project path the decision is recorded against.
	ProjectDir string
	// SettingsFile is the project settings path, named in every message.
	SettingsFile string
	// Document is the project settings document as read, before any merge: the global file
	// is the user's own and needs no confirmation.
	Document config.Document
	Store    *Store
	// Interactive reports whether there is a terminal to ask on.
	Interactive bool
	// PreApproved accepts the grants without asking (`--trust-project`).
	PreApproved bool
	In          io.Reader
	Out         io.Writer
}

// Gate refuses to start when a project's own settings ask for capabilities the user has not
// accepted for that project. A project with no gated settings never prompts and records
// nothing.
//
// It must run before the settings snapshot reaches the instance registry: teardown replays
// `cleanupHost` from that snapshot, so a refused start that had already recorded one would
// run the very script it declined. The same snapshot is what protects an accepted run — the
// agent can edit the project file it lives in, but teardown replays what was trusted at start.
func Gate(opts Options) error {
	grants, err := requestedGrants(opts.Document)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	digest := digestOf(grants)
	keys := strings.Join(grantKeys(grants), ", ")

	if opts.Store.Trusted(opts.ProjectDir, digest) {
		logging.Debug("project settings in %s are trusted (%s)", opts.SettingsFile, keys)
		return nil
	}

	switch {
	case opts.PreApproved:
		logging.Warn("trusting project settings in %s (%s) because --trust-project was given",
			opts.SettingsFile, keys)
	case !opts.Interactive:
		return fmt.Errorf(
			"project settings in %s ask for access beyond the sandbox (%s) and this project has not "+
				"been trusted; "+
				"there is no terminal to confirm on, so re-run from a terminal or pass --trust-project",
			opts.SettingsFile, keys)
	default:
		describe(opts.Out, opts.SettingsFile, grants)
		if !confirm(opts.In, opts.Out) {
			return fmt.Errorf("project settings in %s were not trusted; nothing was started", opts.SettingsFile)
		}
		logging.Info("Trusting project settings in %s (%s)", opts.SettingsFile, keys)
	}

	// A recorded decision is a convenience, not the permission itself: the user has already
	// given it for this run, so failing to remember it must not fail the start.
	if err := opts.Store.Trust(opts.ProjectDir, digest, grantKeys(grants)); err != nil {
		logging.Warn("could not record the trust decision in %s: %v — delete that file to let Hole "+
			"rebuild it, and you will simply be asked again", opts.Store.Path(), err)
	}
	return nil
}

// describe prints what a project's settings ask for, one line per requested value.
func describe(out io.Writer, settingsFile string, grants []grant) {
	fmt.Fprintf(out, "\n  The project's own settings ask for access beyond the sandbox:\n")
	fmt.Fprintf(out, "  %s\n\n", settingsFile)
	for _, granted := range grants {
		fmt.Fprintf(out, "    %s — %s\n", granted.Key, granted.Effect)
		for _, value := range granted.Values {
			fmt.Fprintf(out, "        %s\n", value)
		}
	}
	fmt.Fprintf(out, "\n  Trust them only if you trust this repository's contents.\n")
}

// confirm asks the yes/no question. Anything but an explicit yes is a no.
func confirm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "\n  Trust this project? [y/N] ")
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && answer == "" {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
