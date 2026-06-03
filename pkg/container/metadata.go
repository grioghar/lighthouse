package container

import "strconv"

// Lighthouse reads its configuration from container labels. As of the fork from
// containrrr/watchtower the canonical namespace is "lighthouse.*", but the
// legacy "com.centurylinklabs.watchtower.*" labels are still honoured (see
// legacyLabelFor) so existing watchtower deployments keep working unchanged.
// When both are present, the new lighthouse label takes precedence.
const (
	lighthouseLabel        = "lighthouse"
	signalLabel            = "lighthouse.stop-signal"
	enableLabel            = "lighthouse.enable"
	monitorOnlyLabel       = "lighthouse.monitor-only"
	noPullLabel            = "lighthouse.no-pull"
	dependsOnLabel         = "lighthouse.depends-on"
	scope                  = "lighthouse.scope"
	preCheckLabel          = "lighthouse.lifecycle.pre-check"
	postCheckLabel         = "lighthouse.lifecycle.post-check"
	preUpdateLabel         = "lighthouse.lifecycle.pre-update"
	postUpdateLabel        = "lighthouse.lifecycle.post-update"
	preUpdateTimeoutLabel  = "lighthouse.lifecycle.pre-update-timeout"
	postUpdateTimeoutLabel = "lighthouse.lifecycle.post-update-timeout"

	// Legacy identity label inherited from watchtower. Kept separate because it
	// has no namespaced suffix.
	watchtowerLabel = "com.centurylinklabs.watchtower"

	// zodiacLabel predates watchtower and has no lighthouse equivalent.
	zodiacLabel = "com.centurylinklabs.zodiac.original-image"
)

// legacyLabelFor maps each canonical lighthouse label to the equivalent legacy
// watchtower label that is still recognised as a fallback.
var legacyLabelFor = map[string]string{
	signalLabel:            "com.centurylinklabs.watchtower.stop-signal",
	enableLabel:            "com.centurylinklabs.watchtower.enable",
	monitorOnlyLabel:       "com.centurylinklabs.watchtower.monitor-only",
	noPullLabel:            "com.centurylinklabs.watchtower.no-pull",
	dependsOnLabel:         "com.centurylinklabs.watchtower.depends-on",
	scope:                  "com.centurylinklabs.watchtower.scope",
	preCheckLabel:          "com.centurylinklabs.watchtower.lifecycle.pre-check",
	postCheckLabel:         "com.centurylinklabs.watchtower.lifecycle.post-check",
	preUpdateLabel:         "com.centurylinklabs.watchtower.lifecycle.pre-update",
	postUpdateLabel:        "com.centurylinklabs.watchtower.lifecycle.post-update",
	preUpdateTimeoutLabel:  "com.centurylinklabs.watchtower.lifecycle.pre-update-timeout",
	postUpdateTimeoutLabel: "com.centurylinklabs.watchtower.lifecycle.post-update-timeout",
}

// GetLifecyclePreCheckCommand returns the pre-check command set in the container metadata or an empty string
func (c Container) GetLifecyclePreCheckCommand() string {
	return c.getLabelValueOrEmpty(preCheckLabel)
}

// GetLifecyclePostCheckCommand returns the post-check command set in the container metadata or an empty string
func (c Container) GetLifecyclePostCheckCommand() string {
	return c.getLabelValueOrEmpty(postCheckLabel)
}

// GetLifecyclePreUpdateCommand returns the pre-update command set in the container metadata or an empty string
func (c Container) GetLifecyclePreUpdateCommand() string {
	return c.getLabelValueOrEmpty(preUpdateLabel)
}

// GetLifecyclePostUpdateCommand returns the post-update command set in the container metadata or an empty string
func (c Container) GetLifecyclePostUpdateCommand() string {
	return c.getLabelValueOrEmpty(postUpdateLabel)
}

// ContainsWatchtowerLabel takes a map of labels and values and tells
// the consumer whether it contains a valid lighthouse (or legacy watchtower)
// instance label
func ContainsWatchtowerLabel(labels map[string]string) bool {
	if val, ok := labels[lighthouseLabel]; ok && val == "true" {
		return true
	}
	val, ok := labels[watchtowerLabel]
	return ok && val == "true"
}

func (c Container) getLabelValueOrEmpty(label string) string {
	if val, ok := c.getLabelValue(label); ok {
		return val
	}
	return ""
}

// getLabelValue looks up the canonical lighthouse label and, if it is not
// present, falls back to the equivalent legacy watchtower label.
func (c Container) getLabelValue(label string) (string, bool) {
	labels := c.containerInfo.Config.Labels
	if val, ok := labels[label]; ok {
		return val, true
	}
	if legacy, ok := legacyLabelFor[label]; ok {
		if val, ok := labels[legacy]; ok {
			return val, true
		}
	}
	return "", false
}

func (c Container) getBoolLabelValue(label string) (bool, error) {
	if strVal, ok := c.getLabelValue(label); ok {
		value, err := strconv.ParseBool(strVal)
		return value, err
	}
	return false, errorLabelNotFound
}
