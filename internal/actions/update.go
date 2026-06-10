package actions

import (
	"errors"
	"fmt"
	"time"

	"github.com/grioghar/lighthouse/internal/util"
	"github.com/grioghar/lighthouse/pkg/container"
	"github.com/grioghar/lighthouse/pkg/lifecycle"
	"github.com/grioghar/lighthouse/pkg/session"
	"github.com/grioghar/lighthouse/pkg/sorter"
	"github.com/grioghar/lighthouse/pkg/types"
	log "github.com/sirupsen/logrus"
)

// defaultHealthTimeout is how long a health-gated update waits for an updated
// container to become healthy before rolling back, when no timeout is provided.
const defaultHealthTimeout = 60 * time.Second

// Update looks at the running Docker containers to see if any of the images
// used to start those containers have been updated. If a change is detected in
// any of the images, the associated containers are stopped and restarted with
// the new image.
func Update(client container.Client, params types.UpdateParams) (types.Report, error) {
	log.Debug("Checking containers for updated images")
	progress := &session.Progress{}
	staleCount := 0

	if params.LifecycleHooks {
		lifecycle.ExecutePreChecks(client, params)
	}

	containers, err := client.ListContainers(params.Filter)
	if err != nil {
		return nil, err
	}

	staleCheckFailed := 0

	for i, targetContainer := range containers {
		stale, newestImage, err := client.IsContainerStale(targetContainer, params)
		shouldUpdate := stale && !params.NoRestart && !targetContainer.IsMonitorOnly(params)
		if err == nil && shouldUpdate {
			// Check to make sure we have all the necessary information for recreating the container
			err = targetContainer.VerifyConfiguration()
			// If the image information is incomplete and trace logging is enabled, log it for further diagnosis
			if err != nil && log.IsLevelEnabled(log.TraceLevel) {
				imageInfo := targetContainer.ImageInfo()
				log.Tracef("Image info: %#v", imageInfo)
				log.Tracef("Container info: %#v", targetContainer.ContainerInfo())
				if imageInfo != nil {
					log.Tracef("Image config: %#v", imageInfo.Config)
				}
			}
		}

		if err != nil {
			log.Infof("Unable to update container %q: %v. Proceeding to next.", targetContainer.Name(), err)
			stale = false
			staleCheckFailed++
			progress.AddSkipped(targetContainer, err)
		} else {
			progress.AddScanned(targetContainer, newestImage)
		}
		containers[i].SetStale(stale)

		if stale {
			staleCount++
		}
	}

	containers, err = sorter.SortByDependencies(containers)
	if err != nil {
		return nil, err
	}

	UpdateImplicitRestart(containers)

	var containersToUpdate []types.Container
	for _, c := range containers {
		if !c.IsMonitorOnly(params) {
			containersToUpdate = append(containersToUpdate, c)
			progress.MarkForUpdate(c.ID())
		}
	}

	if params.RollingRestart {
		progress.UpdateFailed(performRollingRestart(containersToUpdate, client, params))
	} else {
		failedStop, stoppedImages := stopContainersInReversedOrder(containersToUpdate, client, params)
		progress.UpdateFailed(failedStop)
		failedStart := restartContainersInSortedOrder(containersToUpdate, client, params, stoppedImages)
		progress.UpdateFailed(failedStart)
	}

	if params.LifecycleHooks {
		lifecycle.ExecutePostChecks(client, params)
	}
	return progress.Report(), nil
}

func performRollingRestart(containers []types.Container, client container.Client, params types.UpdateParams) map[types.ContainerID]error {
	cleanupImageIDs := make(map[types.ImageID]bool, len(containers))
	failed := make(map[types.ContainerID]error, len(containers))

	for i := len(containers) - 1; i >= 0; i-- {
		if containers[i].ToRestart() {
			err := stopStaleContainer(containers[i], client, params)
			if err != nil {
				failed[containers[i].ID()] = err
			} else {
				if err := restartStaleContainer(containers[i], client, params); err != nil {
					failed[containers[i].ID()] = err
				} else if containers[i].IsStale() {
					// Only add (previously) stale containers' images to cleanup
					cleanupImageIDs[containers[i].ImageID()] = true
				}
			}
		}
	}

	if params.Cleanup {
		cleanupImages(client, cleanupImageIDs)
	}
	return failed
}

func stopContainersInReversedOrder(containers []types.Container, client container.Client, params types.UpdateParams) (failed map[types.ContainerID]error, stopped map[types.ImageID]bool) {
	failed = make(map[types.ContainerID]error, len(containers))
	stopped = make(map[types.ImageID]bool, len(containers))
	for i := len(containers) - 1; i >= 0; i-- {
		if err := stopStaleContainer(containers[i], client, params); err != nil {
			failed[containers[i].ID()] = err
		} else {
			// NOTE: If a container is restarted due to a dependency this might be empty
			stopped[containers[i].SafeImageID()] = true
		}

	}
	return
}

func stopStaleContainer(container types.Container, client container.Client, params types.UpdateParams) error {
	if container.IsWatchtower() {
		log.Debugf("This is the lighthouse container %s", container.Name())
		return nil
	}

	if !container.ToRestart() {
		return nil
	}

	// Perform an additional check here to prevent us from stopping a linked container we cannot restart
	if container.IsLinkedToRestarting() {
		if err := container.VerifyConfiguration(); err != nil {
			return err
		}
	}

	if params.LifecycleHooks {
		skipUpdate, err := lifecycle.ExecutePreUpdateCommand(client, container)
		if err != nil {
			log.Error(err)
			log.Info("Skipping container as the pre-update command failed")
			return err
		}
		if skipUpdate {
			log.Debug("Skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
			return errors.New("skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
		}
	}

	if err := client.StopContainer(container, params.Timeout); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func restartContainersInSortedOrder(containers []types.Container, client container.Client, params types.UpdateParams, stoppedImages map[types.ImageID]bool) map[types.ContainerID]error {
	cleanupImageIDs := make(map[types.ImageID]bool, len(containers))
	failed := make(map[types.ContainerID]error, len(containers))

	for _, c := range containers {
		if !c.ToRestart() {
			continue
		}
		if stoppedImages[c.SafeImageID()] {
			if err := restartStaleContainer(c, client, params); err != nil {
				failed[c.ID()] = err
			} else if c.IsStale() {
				// Only add (previously) stale containers' images to cleanup
				cleanupImageIDs[c.ImageID()] = true
			}
		}
	}

	if params.Cleanup {
		cleanupImages(client, cleanupImageIDs)
	}

	return failed
}

func cleanupImages(client container.Client, imageIDs map[types.ImageID]bool) {
	for imageID := range imageIDs {
		if imageID == "" {
			continue
		}
		if err := client.RemoveImageByID(imageID); err != nil {
			log.Error(err)
		}
	}
}

func restartStaleContainer(container types.Container, client container.Client, params types.UpdateParams) error {
	// Since we can't shutdown a lighthouse container immediately, we need to
	// start the new one while the old one is still running. This prevents us
	// from re-using the same container name so we first rename the current
	// instance so that the new one can adopt the old name.
	if container.IsWatchtower() {
		if err := client.RenameContainer(container, util.RandName()); err != nil {
			log.Error(err)
			return nil
		}
	}

	if !params.NoRestart {
		newContainerID, err := client.StartContainer(container)
		if err != nil {
			log.Error(err)
			return err
		}
		if container.ToRestart() && params.LifecycleHooks {
			lifecycle.ExecutePostUpdateCommand(client, newContainerID)
		}
		// Health-gated rollback: if an updated container fails to come up
		// healthy, restore the previous image so a bad release can't take a
		// service down. The lighthouse container itself is excluded since it
		// can't supervise its own replacement.
		if params.HealthGated && container.IsStale() && !container.IsWatchtower() {
			if err := verifyHealthOrRollback(client, container, newContainerID, params); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyHealthOrRollback waits for the freshly-started container to become
// healthy. If it does not, it stops the unhealthy container and recreates it
// from the previous image, returning an error so the update is reported as
// failed (and the previous image is not cleaned up).
func verifyHealthOrRollback(client container.Client, c types.Container, newContainerID types.ContainerID, params types.UpdateParams) error {
	clog := log.WithField("container", c.Name())
	if waitForContainerHealth(client, newContainerID, params.HealthTimeout) {
		clog.Debug("Updated container reported healthy")
		return nil
	}

	previousImage := c.ImageID()
	clog.Warnf("Updated container did not become healthy; rolling back to previous image %s", previousImage.ShortID())

	if newContainer, err := client.GetContainer(newContainerID); err != nil {
		clog.WithError(err).Error("Rollback: could not inspect the unhealthy container")
	} else if err := client.StopContainer(newContainer, params.Timeout); err != nil {
		clog.WithError(err).Error("Rollback: could not stop the unhealthy container")
	}

	if previousImage == "" {
		return fmt.Errorf("container %q was unhealthy after update and no previous image was available to roll back to", c.Name())
	}

	if _, err := client.StartContainerWithImage(c, previousImage); err != nil {
		return fmt.Errorf("container %q was unhealthy after update and rollback to %s failed: %w", c.Name(), previousImage.ShortID(), err)
	}

	return fmt.Errorf("container %q was unhealthy after update; rolled back to previous image %s", c.Name(), previousImage.ShortID())
}

// waitForContainerHealth polls the container until it is healthy, fails, or the
// timeout elapses. For images with a HEALTHCHECK it honours the reported health
// status; for images without one it falls back to "running and not restarting"
// once the timeout has elapsed, which catches crash-looping containers.
func waitForContainerHealth(client container.Client, id types.ContainerID, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	const interval = 2 * time.Second
	deadline := time.Now().Add(timeout)

	for {
		hs, err := client.GetContainerHealth(id)
		if err != nil {
			log.WithError(err).Debug("Health check inspection failed")
			return false
		}

		switch hs.Status {
		case types.HealthHealthy:
			return true
		case types.HealthUnhealthy:
			return false
		}

		timedOut := !time.Now().Before(deadline)
		if hs.Status == "" {
			// No HEALTHCHECK defined: judge by run state once settled.
			if timedOut {
				return hs.Running && !hs.Restarting
			}
		} else if timedOut {
			// Still "starting" at the deadline -> treat as failed.
			return false
		}

		time.Sleep(interval)
	}
}

// UpdateImplicitRestart iterates through the passed containers, setting the
// `LinkedToRestarting` flag if any of it's linked containers are marked for restart
func UpdateImplicitRestart(containers []types.Container) {

	for ci, c := range containers {
		if c.ToRestart() {
			// The container is already marked for restart, no need to check
			continue
		}

		if link := linkedContainerMarkedForRestart(c.Links(), containers); link != "" {
			log.WithFields(log.Fields{
				"restarting": link,
				"linked":     c.Name(),
			}).Debug("container is linked to restarting")
			// NOTE: To mutate the array, the `c` variable cannot be used as it's a copy
			containers[ci].SetLinkedToRestarting(true)
		}

	}
}

// linkedContainerMarkedForRestart returns the name of the first link that matches a
// container marked for restart
func linkedContainerMarkedForRestart(links []string, containers []types.Container) string {
	for _, linkName := range links {
		for _, candidate := range containers {
			if candidate.Name() == linkName && candidate.ToRestart() {
				return linkName
			}
		}
	}
	return ""
}
