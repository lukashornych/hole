package sandbox

import (
	"strings"

	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/image"
	"github.com/lukashornych/hole/internal/logging"
)

// collectImages bounds each agent image repository to the one tag in use.
//
// It runs only *after* the agent service came up: a failed build must never have destroyed
// the last working image. Every removal is best-effort — an image a concurrent sandbox is
// using survives until a later pass.
func collectImages(containerEngine *engine.Engine, projectName string, identity image.Identity, gatewayImage string) {
	// 1. Other tags of the chosen repository: each configuration generation supersedes the
	//    previous one, so only the current tag is worth keeping.
	//
	//    For the shared repository a superseded tag can belong to another project's older
	//    generation. That is deliberate: an image a running sandbox uses cannot be removed, and
	//    the worst outcome for a stopped one is that its next start rebuilds.
	removeOtherTags(containerEngine, identity.Repository, identity.Tag)

	// 2. If the shared image was chosen, the project's own repository is obsolete — it needed
	//    a custom image before and does not any more.
	if identity.Scope == image.ScopeGlobal {
		removeOtherTags(containerEngine, image.AgentRepository(projectName), "")
	}

	// 3. Gateway images from earlier Hole versions. The tag follows the embedded assets, so an
	//    upgrade supersedes the previous one exactly like an agent image generation.
	if _, tag, found := strings.Cut(gatewayImage, ":"); found {
		removeOtherTags(containerEngine, image.GatewayRepository, tag)
	}

	// 4. Reclaim dangling images a re-tag left behind. The label restricts the prune to Hole's
	//    own images, so a user's unrelated dangling images are never touched.
	if err := containerEngine.ImagePruneDangling(image.ImageLabel); err != nil {
		logging.Debug("dangling image prune failed (unsupported filter combination?): %v", err)
	}
}

// removeOtherTags removes every tag of a repository except keepTag ("" keeps none).
func removeOtherTags(containerEngine *engine.Engine, repository, keepTag string) {
	for _, reference := range containerEngine.ImagesByReference(repository) {
		_, tag, found := strings.Cut(reference, ":")
		if !found || (keepTag != "" && tag == keepTag) {
			continue
		}
		if err := containerEngine.ImageRemove(reference); err != nil {
			logging.Debug("could not remove superseded image %s: %v", reference, err)
			continue
		}
		logging.Debug("removed superseded image %s", reference)
	}
}
