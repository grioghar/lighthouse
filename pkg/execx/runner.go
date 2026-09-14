// Package execx is the one place Lighthouse decides *where* a command lands.
//
// Lighthouse has to reach four different places, often in the same run: the
// machine it is running on, a Proxmox node it is not running on, the inside of
// an LXC guest, and the inside of a Docker container. Every update backend
// needs all of that and none of them should care how it is arranged, so the
// difference is confined to a Runner rather than spread through the callers.
//
// The interface reports the exit code separately from the error because
// package managers use the exit status as data, not as failure: `dnf
// check-update` exits 100 precisely when updates exist, and `zypper
// needs-rebooting` exits 102 when a reboot is due. A Runner that folded those
// into an error would make the common case indistinguishable from a broken
// command. So err is non-nil only when the command could not be run or did not
// finish -- spawn failure, timeout, lost connection -- and a command that ran
// and exited non-zero returns a Result with that Code and a nil error.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds any single command. Long operations (image pulls,
// guest creation) are launched as Proxmox tasks and polled, so no individual
// call should approach this.
const DefaultTimeout = 2 * time.Minute

// Result is the outcome of a command that ran to completion.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// OK reports whether the command exited zero.
func (r Result) OK() bool { return r.Code == 0 }

// Out returns trimmed stdout, which is what nearly every caller wants.
func (r Result) Out() string { return strings.TrimSpace(r.Stdout) }

// Err returns trimmed stderr, falling back to stdout when a tool reports its
// failure on stdout instead (apt and apk both do, in different cases).
func (r Result) Err() string {
	if s := strings.TrimSpace(r.Stderr); s != "" {
		return s
	}
	return strings.TrimSpace(r.Stdout)
}

// Runner executes a command somewhere and returns how it finished.
//
// A non-nil error means the command did not run or did not complete. A
// non-zero exit is reported in Result.Code with a nil error; see the package
// comment for why that distinction is load-bearing.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

// Describer lets a Runner say where it sends commands, for log lines and
// report columns. Runners that do not implement it are described generically.
type Describer interface {
	Where() string
}

// Where describes a Runner's destination.
func Where(r Runner) string {
	if d, ok := r.(Describer); ok {
		return d.Where()
	}
	return "unknown"
}

// Output runs a command and treats a non-zero exit as an error.
//
// Most callers genuinely do want that -- `pvesh get` exiting non-zero is a
// failure, full stop. Only the package-manager backends need the raw code, so
// they use Run directly and everything else uses this.
func Output(ctx context.Context, r Runner, name string, args ...string) (string, error) {
	res, err := r.Run(ctx, name, args...)
	if err != nil {
		return res.Stdout, err
	}
	if !res.OK() {
		return res.Stdout, fmt.Errorf("%s %s: exit %d: %s",
			name, strings.Join(args, " "), res.Code, truncate(res.Err(), 300))
	}
	return res.Stdout, nil
}

// Local runs commands on the machine Lighthouse itself is running on.
type Local struct {
	// Timeout bounds a single command. Zero means DefaultTimeout.
	Timeout time.Duration
}

func (l Local) Where() string { return "local" }

func (l Local) Run(ctx context.Context, name string, args ...string) (Result, error) {
	timeout := l.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}

	var ee *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &ee):
		// Ran and exited non-zero: data, not failure.
		res.Code = ee.ExitCode()
		// A context deadline surfaces as a kill signal (ExitCode -1), which is
		// a genuine failure to complete and must not be reported as data.
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s: %w", name, ctx.Err())
		}
		return res, nil
	default:
		// Could not spawn at all: no such binary, permission denied.
		return res, fmt.Errorf("%s: %w", name, err)
	}
}

// SSH runs commands on another machine.
//
// This is what lets Lighthouse live somewhere other than the Proxmox node. The
// LXC control paths (pct, pvesh) exist only on the node and have no remote
// equivalent, so when Lighthouse runs in a container -- which is the normal
// deployment -- the commands still have to land on the node. Only the process
// moves; the commands do not.
type SSH struct {
	Host    string // e.g. root@10.0.0.1
	KeyFile string // optional; empty uses the caller's ssh config and agent
	Timeout time.Duration
	// Options are extra ssh flags. The defaults below are applied when this is
	// empty; set it to take full control.
	Options []string
}

func (s SSH) Where() string { return "ssh:" + s.Host }

// defaultSSHOptions keep an unattended run from hanging on a prompt.
// BatchMode refuses password prompts instead of blocking forever on a TTY that
// is not there, and the timeouts bound a node that has gone away mid-run.
var defaultSSHOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=4",
}

func (s SSH) Run(ctx context.Context, name string, args ...string) (Result, error) {
	argv := []string{}
	opts := s.Options
	if len(opts) == 0 {
		opts = defaultSSHOptions
	}
	argv = append(argv, opts...)
	if s.KeyFile != "" {
		argv = append(argv, "-i", s.KeyFile)
	}

	// Quote every argument. The remote side always runs the string through a
	// shell, and guest names, image references and package-manager scripts all
	// contain characters it would otherwise split, glob or expand.
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, Quote(name))
	for _, a := range args {
		quoted = append(quoted, Quote(a))
	}
	argv = append(argv, s.Host, strings.Join(quoted, " "))

	res, err := Local{Timeout: s.Timeout}.Run(ctx, "ssh", argv...)
	if err != nil {
		return res, err
	}
	// 255 is ssh's own "the transport failed" code, distinct from any exit
	// status the remote command could return. Reporting it as a remote exit
	// would make an unreachable node look like a command that merely failed.
	if res.Code == 255 {
		return res, fmt.Errorf("ssh %s: %s%s", s.Host, truncate(res.Err(), 300), sshHint(res.Err()))
	}
	return res, nil
}

// sshHint turns ssh's terser failures into the fix, because these are the two
// that bite every containerised deployment on first run and neither message
// says what to do about it.
func sshHint(stderr string) string {
	l := strings.ToLower(stderr)
	switch {
	case strings.Contains(l, "host key verification failed"),
		strings.Contains(l, "no matching host key"),
		strings.Contains(l, "known_hosts"):
		// A fresh container has an empty known_hosts, so the very first
		// connection fails with no way for an unattended run to answer the
		// prompt.
		return " -- the container has no known_hosts entry for this node; " +
			"mount one at /root/.ssh/known_hosts, or seed it with " +
			"`ssh-keyscan <node> >> known_hosts`"
	case strings.Contains(l, "permission denied"),
		strings.Contains(l, "no such identity"),
		strings.Contains(l, "could not open a connection to your authentication agent"):
		// BatchMode refuses to prompt, so a missing key is a hard failure
		// rather than a password prompt nobody is there to answer.
		return " -- no usable key (BatchMode refuses password prompts); " +
			"mount a private key and pass --ssh-key, or bind-mount an agent socket"
	}
	return ""
}

// Quote makes a string safe as a single argument to a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Script wraps a shell script so it can be handed to any Runner.
//
// The package-manager backends are written as shell one-liners because that is
// what they genuinely are -- pipelines of the distro's own tools -- and because
// a single command crosses an ssh hop or a pct exec unchanged.
func Script(ctx context.Context, r Runner, script string) (Result, error) {
	return r.Run(ctx, "sh", "-c", script)
}

// Func adapts a plain function to Runner, for tests and for wrapping.
type Func func(ctx context.Context, name string, args ...string) (Result, error)

func (f Func) Run(ctx context.Context, name string, args ...string) (Result, error) {
	return f(ctx, name, args...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
