package types

import (
	"time"
)

// UpdateParams contains all different options available to alter the behavior of the Update func
type UpdateParams struct {
	Filter          Filter
	Cleanup         bool
	NoRestart       bool
	Timeout         time.Duration
	MonitorOnly     bool
	NoPull          bool
	LifecycleHooks  bool
	RollingRestart  bool
	LabelPrecedence bool
	HealthGated     bool
	HealthTimeout   time.Duration
}

// HealthState is a snapshot of a container's runtime/health status, used to
// decide whether an updated container came up cleanly or should be rolled back.
type HealthState struct {
	// Status mirrors Docker's health status ("healthy", "unhealthy" or
	// "starting"), or is empty when the image defines no HEALTHCHECK.
	Status     string
	Running    bool
	Restarting bool
	ExitCode   int
}

// Health status constants matching Docker's container health values.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthStarting  = "starting"
)
