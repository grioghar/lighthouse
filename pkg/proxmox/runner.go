// Package proxmox adapts Lighthouse's update model to Proxmox VE guests.
//
// Two things can be kept current on a Proxmox node by image, and they share
// almost nothing but a scheduler:
//
//   - OCI-derived LXCs, created from an image pulled with oci-registry-pull.
//     These behave like Docker containers for *detection* -- compare the
//     installed manifest digest to the registry's -- but Proxmox squashes all
//     layers into one rootfs at creation, so there is no in-place update. The
//     only apply path is to recreate the guest.
//   - Conventional LXCs, where "update" means the distribution package
//     manager, not an image digest.
//
// The package-manager half now lives in pkg/pkgmgr and is driven by the
// providers in pkg/providers/pkgprov, so it is shared with the host and with
// Docker containers rather than reimplemented here. What remains in this
// package is the part that is genuinely Proxmox-specific: the node API, the
// OCI template layout, and the guest tagging that records which image a guest
// came from.
package proxmox

import (
	"time"

	"github.com/grioghar/lighthouse/pkg/execx"
)

// Runner executes a command on the Proxmox node and returns how it finished.
//
// This is execx.Runner. Command transport is deliberately one abstraction for
// the whole program: the same interface that reaches a node over ssh also
// reaches into an LXC guest and into a Docker container, and composing those
// is how one package-manager implementation serves all three.
type Runner = execx.Runner

// DefaultTimeout bounds any single node command. Pulls and container creation
// are launched as Proxmox tasks and polled, so no single call should be long.
const DefaultTimeout = execx.DefaultTimeout

// ExecRunner runs commands directly, for a Lighthouse running on the node.
//
// Deprecated: use execx.Local. Retained so existing callers keep compiling.
type ExecRunner = execx.Local

// SSHRunner runs node commands over ssh, for a Lighthouse running elsewhere.
// The LXC paths still require the commands to land on the node; this only
// moves where the Lighthouse process itself lives.
//
// Deprecated: use execx.SSH.
type SSHRunner = execx.SSH

// NewRunner builds the right transport for a node.
//
// This is the decision every entry point otherwise repeats: ssh when a host is
// given, local when it is not. It is one line, but getting it wrong is the
// single most common misconfiguration -- pct and pvesh exist only on the node,
// so a containerised Lighthouse with no ssh host silently has no way to reach
// any guest.
func NewRunner(sshHost, sshKey string, timeout time.Duration) Runner {
	if sshHost != "" {
		return execx.SSH{Host: sshHost, KeyFile: sshKey, Timeout: timeout}
	}
	return execx.Local{Timeout: timeout}
}
