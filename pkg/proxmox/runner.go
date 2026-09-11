// Package proxmox adapts Lighthouse's update model to Proxmox VE guests.
//
// Three things can be kept current on a Proxmox node, and they share almost
// nothing but a scheduler:
//
//   - OCI-derived LXCs, created from an image pulled with oci-registry-pull.
//     These behave like Docker containers for *detection* -- compare the
//     installed manifest digest to the registry's -- but Proxmox squashes all
//     layers into one rootfs at creation, so there is no in-place update. The
//     only apply path is to recreate the guest.
//   - Conventional LXCs (ostype debian/ubuntu/...), where "update" means the
//     distribution package manager, not an image digest.
//   - Neither, which is most guests; those are left alone.
//
// Mode selects which of those this run touches, so an operator can start with
// detection only and opt into the destructive paths deliberately.
package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner executes a command on the Proxmox node and returns its stdout.
//
// It exists so the API surface can be driven locally (pvesh/pct on the node),
// over ssh, or by a fake in tests, without the callers caring. Lighthouse has
// to run on the node itself for the LXC paths: pct has no remote equivalent.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs commands directly, for a Lighthouse running on the node.
type ExecRunner struct {
	// Timeout bounds a single command. Zero means DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds any single node command. Pulls and container creation
// are launched as Proxmox tasks and polled, so no single call should be long.
const DefaultTimeout = 2 * time.Minute

func (e ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	timeout := e.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

// SSHRunner runs node commands over ssh, for a Lighthouse running elsewhere.
// The LXC paths still require the commands to land on the node; this only moves
// where the Lighthouse process itself lives.
type SSHRunner struct {
	Host    string // e.g. root@10.0.0.1
	KeyFile string // optional; empty uses the caller's ssh config and agent
	Timeout time.Duration
}

func (s SSHRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	ssh := []string{}
	if s.KeyFile != "" {
		ssh = append(ssh, "-i", s.KeyFile)
	}
	// Quote each argument: guest names and image references contain characters
	// the remote shell would otherwise split or expand.
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, shellQuote(name))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	ssh = append(ssh, s.Host, strings.Join(quoted, " "))
	return ExecRunner{Timeout: s.Timeout}.Run(ctx, "ssh", ssh...)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
