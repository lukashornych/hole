package sandbox

import (
	"sort"
	"strings"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/image"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// adoptExistingImage re-tags a bit-identical image into this project's repository so compose
// finds it present and skips the build.
//
// The tag hashes the closed set of build inputs — configuration, host identity, embedded build
// assets, agent and setup-script contents — so two agent images carrying it were produced from
// identical inputs regardless of which repository they landed in. Only the repository name is
// path-keyed, which is why sibling checkouts of one project otherwise rebuild an image they
// already have.
//
// Best-effort: any failure just leaves compose to build as it did before.
func adoptExistingImage(containerEngine *engine.Engine, identity image.Identity) bool {
	target := identity.Reference()
	if containerEngine.ImageExists(target) {
		return false
	}

	source := selectAdoptable(containerEngine.ImagesByReference(image.AgentRepositoryPrefix+"*:"+identity.Tag), target)
	if source == "" {
		return false
	}

	if err := containerEngine.ImageTag(source, target); err != nil {
		logging.Debug("could not adopt %s as %s: %v", source, target, err)
		return false
	}
	logging.Debug("adopted %s as %s", source, target)
	return true
}

// selectAdoptable picks a reference to re-tag: any agent image already carrying the target tag,
// other than the target itself. Candidates are sorted first, so the pick is deterministic.
func selectAdoptable(candidates []string, target string) string {
	sorted := append([]string(nil), candidates...)
	sort.Strings(sorted)
	for _, candidate := range sorted {
		if sameReference(candidate, target) {
			continue
		}
		return candidate
	}
	return ""
}

// sameReference reports whether a listed candidate names the target repository and tag. Podman
// reports its own images with a "localhost/" registry prefix that docker omits, so a plain
// comparison would fail to recognise the target among the candidates.
func sameReference(candidate, target string) bool {
	return candidate == target || strings.HasSuffix(candidate, "/"+target)
}
