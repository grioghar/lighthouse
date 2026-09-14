package execx

import (
	"context"
	"strconv"
	"strings"
)

// The runners below are composed over a base Runner rather than replacing it.
// That composition is the whole point: "inside guest 3117, on a node I reach
// over ssh" is Guest{Base: SSH{...}}, and no backend that uses it has to know
// either half. It is also what makes the same package-manager code work
// against an LXC guest, a Docker container and a bare host without branching.

// Guest runs commands inside a Proxmox LXC guest via `pct exec`.
//
// pct exists only on the node, so Base must already land there -- Local when
// Lighthouse runs on the node, SSH when it does not.
type Guest struct {
	Base Runner
	VMID int
}

func (g Guest) Where() string {
	return Where(g.Base) + "/ct:" + strconv.Itoa(g.VMID)
}

func (g Guest) Run(ctx context.Context, name string, args ...string) (Result, error) {
	// `pct exec <id> -- cmd args` execs directly, with no shell on the guest
	// side, so arguments arrive exactly as given. Callers that want a shell ask
	// for one explicitly via Script.
	argv := append([]string{"exec", strconv.Itoa(g.VMID), "--", name}, args...)
	res, err := g.Base.Run(ctx, "pct", argv...)
	if err != nil {
		return res, err
	}
	// pct reports its own failures (guest not running, locked, no such guest)
	// on stderr with a non-zero exit, which is indistinguishable by code alone
	// from the guest command failing. Recognising them keeps "the guest is
	// stopped" from being reported as "apt-get failed".
	if !res.OK() && isPctFailure(res.Err()) {
		return res, &NestedError{Where: g.Where(), Detail: truncate(res.Err(), 200)}
	}
	return res, nil
}

func isPctFailure(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range []string{
		"does not exist",
		"not running",
		"is locked",
		"configuration file",
		"permission denied",
		"command 'pct' not found",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// Container runs commands inside a Docker container via `docker exec`.
//
// This is for reaching *into* a container's package manager, which is a
// different thing from Lighthouse's Docker path: that one replaces a container
// with a newer image and never runs a package manager. This exists for the
// cases where an image is not the unit of update -- a long-lived container
// nobody rebuilds, or one whose image is not tracked.
type Container struct {
	Base Runner
	// ID is the container name or id.
	ID string
	// User optionally runs as a specific user; empty uses the image default.
	// Package managers generally need root, and an image whose default user is
	// unprivileged will fail without this.
	User string
}

func (c Container) Where() string { return Where(c.Base) + "/docker:" + c.ID }

func (c Container) Run(ctx context.Context, name string, args ...string) (Result, error) {
	argv := []string{"exec"}
	if c.User != "" {
		argv = append(argv, "--user", c.User)
	}
	argv = append(argv, c.ID, name)
	argv = append(argv, args...)
	res, err := c.Base.Run(ctx, "docker", argv...)
	if err != nil {
		return res, err
	}
	// 126/127 from `docker exec` mean the binary was not executable or not
	// found in the container. That is routine -- most images have no package
	// manager at all -- so it must be distinguishable from a real failure.
	if res.Code == 126 || res.Code == 127 {
		return res, &NestedError{Where: c.Where(), Detail: truncate(res.Err(), 200), NotFound: true}
	}
	if !res.OK() && strings.Contains(strings.ToLower(res.Err()), "is not running") {
		return res, &NestedError{Where: c.Where(), Detail: truncate(res.Err(), 200)}
	}
	return res, nil
}

// NestedError means the container or guest could not be entered, as opposed to
// a command inside it having failed. Backends use this to skip a target
// cleanly instead of reporting it as broken.
type NestedError struct {
	Where    string
	Detail   string
	NotFound bool // the command does not exist in the target
}

func (e *NestedError) Error() string {
	return e.Where + ": " + e.Detail
}
