package sandbox

import (
	"github.com/lukashornych/hole/v2/internal/dindregistry"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/image"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// Destroy removes every Hole resource belonging to one project.
func Destroy(projectDir string) error {
	containerEngine, err := engine.Detect()
	if err != nil {
		return err
	}
	projectName := hostenv.ProjectName(projectDir)

	logging.Info("Destroying cached resources for project: %s", projectDir)
	logging.Info("Project name: %s", projectName)
	logging.Line()

	instancePrefix := "hole-sandbox-" + projectName + "-"
	removeContainers(containerEngine, instancePrefix)
	removeNetworks(containerEngine, instancePrefix)
	// Only this project's own repository: the shared agent image and the gateway may serve
	// other projects.
	removeImages(containerEngine, image.AgentRepository(projectName))
	removeVolumes(containerEngine, "hole-sandbox-docker-data-"+instancePrefix)

	logging.Line()
	logging.Info("Cached resources destroyed. The shared agent and gateway images were preserved (other projects may use them).")
	return nil
}

// DestroyAll removes every Hole resource across all projects.
func DestroyAll() error {
	containerEngine, err := engine.Detect()
	if err != nil {
		return err
	}

	logging.Info("Destroying all Hole Docker resources...")
	logging.Line()

	// The image cache is cheap to rebuild, so a full destroy takes it too.
	dindregistry.Remove(containerEngine)

	removeContainers(containerEngine, "hole-sandbox-")
	removeNetworks(containerEngine, "hole-sandbox-")
	removeImages(containerEngine, "hole-sandbox/*")
	removeVolumes(containerEngine, "hole-sandbox-")

	logging.Line()
	logging.Info("All Hole Docker resources destroyed.")
	return nil
}

func removeContainers(containerEngine *engine.Engine, nameFilter string) {
	ids := containerEngine.ContainerIDs(nameFilter, true)
	if len(ids) == 0 {
		logging.Info("No containers found")
		return
	}
	logging.Info("Removing %d container(s)...", len(ids))
	if err := containerEngine.ContainerRemove(ids...); err != nil {
		logging.Warn("failed to remove some containers: %v", err)
	}
}

func removeNetworks(containerEngine *engine.Engine, nameFilter string) {
	names := containerEngine.NetworkNames(nameFilter)
	if len(names) == 0 {
		logging.Info("No networks found")
		return
	}
	logging.Info("Removing %d network(s)...", len(names))
	for _, name := range names {
		if err := containerEngine.NetworkRemove(name); err != nil {
			logging.Warn("failed to remove network %s", name)
		}
	}
}

func removeImages(containerEngine *engine.Engine, reference string) {
	references := containerEngine.ImagesByReference(reference)
	if len(references) == 0 {
		logging.Info("No images found")
		return
	}
	logging.Info("Removing %d image(s)...", len(references))
	for _, ref := range references {
		if err := containerEngine.ImageRemove(ref); err != nil {
			logging.Warn("failed to remove image %s (it may be in use by a running sandbox)", ref)
		}
	}
}

func removeVolumes(containerEngine *engine.Engine, nameFilter string) {
	names := containerEngine.VolumesByName(nameFilter)
	if len(names) == 0 {
		return
	}
	logging.Info("Removing %d volume(s)...", len(names))
	for _, name := range names {
		if err := containerEngine.VolumeRemove(name); err != nil {
			logging.Warn("failed to remove volume %s", name)
		}
	}
}
