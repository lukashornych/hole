//go:build integration

// Integration test for image adoption. It stands a cheap public image in for a real sandbox
// image — adoption only ever re-tags, so what the image contains is irrelevant. Run it with
// `make itest`.
package sandbox

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/image"
)

const adoptTestTag = "holeadopt00"

func imageID(t *testing.T, containerEngine *engine.Engine, reference string) string {
	t.Helper()
	out, err := exec.Command(containerEngine.Binary, "image", "inspect", "--format", "{{.Id}}", reference).Output()
	if err != nil {
		t.Fatalf("inspect %s: %v", reference, err)
	}
	return strings.TrimSpace(string(out))
}

func TestAdoptExistingImageReTagsASiblingRepository(t *testing.T) {
	containerEngine := testEngine(t)

	existing := image.AgentRepository("adopt-a-11111111") + ":" + adoptTestTag
	identity := image.Identity{
		Repository: image.AgentRepository("adopt-b-22222222"),
		Tag:        adoptTestTag,
		Scope:      image.ScopeProject,
	}
	t.Cleanup(func() {
		_ = containerEngine.ImageRemove(identity.Reference())
		_ = containerEngine.ImageRemove(existing)
	})

	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull the stand-in image: %v", err)
	}
	if err := containerEngine.ImageTag("alpine:3.19", existing); err != nil {
		t.Fatalf("ImageTag: %v", err)
	}

	if !adoptExistingImage(containerEngine, identity) {
		t.Fatal("adoptExistingImage returned false with a sibling repository carrying the tag")
	}
	if !containerEngine.ImageExists(identity.Reference()) {
		t.Fatal("adopted reference is not present")
	}
	if got, want := imageID(t, containerEngine, identity.Reference()), imageID(t, containerEngine, existing); got != want {
		t.Errorf("adopted image ID = %s, want %s", got, want)
	}

	// Adoption is a no-op once the target resolves, so a repeated start never re-tags.
	if adoptExistingImage(containerEngine, identity) {
		t.Error("adoptExistingImage adopted again although the target already exists")
	}

	// Untagging the source leaves the adopted reference usable — this is what keeps
	// collectImages safe without modification.
	if err := containerEngine.ImageRemove(existing); err != nil {
		t.Fatalf("ImageRemove: %v", err)
	}
	if !containerEngine.ImageExists(identity.Reference()) {
		t.Error("removing the source tag took the adopted image with it")
	}
}

func TestAdoptExistingImageWithoutACandidate(t *testing.T) {
	containerEngine := testEngine(t)

	identity := image.Identity{
		Repository: image.AgentRepository("adopt-c-33333333"),
		Tag:        adoptTestTag + "none",
		Scope:      image.ScopeProject,
	}
	t.Cleanup(func() { _ = containerEngine.ImageRemove(identity.Reference()) })

	if adoptExistingImage(containerEngine, identity) {
		t.Error("adoptExistingImage adopted although no image carries the tag")
	}
}
