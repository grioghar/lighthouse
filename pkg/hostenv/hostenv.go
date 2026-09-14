// Package hostenv works out where Lighthouse itself is running, and what it
// can therefore reach directly.
//
// This exists because the same binary is deployed four ways and each one has a
// different set of things within arm's reach:
//
//	bare / PVE node   pct, pvesh and the host's own package manager are local.
//	Docker container  the Docker socket is usually bind-mounted; pct is not
//	                  present and cannot be, so node work must go over ssh.
//	LXC guest         same as Docker minus the socket, unless one is mounted.
//	OCI guest         an LXC guest by another name, as far as this is concerned;
//	                  Proxmox squashes the image into a normal container.
//
// Getting this wrong produces a confusing failure: "pct: command not found"
// from inside a container reads like a broken install, when the real answer is
// that pct can never be there and the deployment needs --ssh-host. Detecting
// it lets Lighthouse say that, and lets the common cases need no flags at all.
package hostenv

import (
	"context"
	"os"
	"strings"

	"github.com/grioghar/lighthouse/pkg/execx"
)

// Kind is the sort of place Lighthouse is running.
type Kind string

const (
	KindBare   Kind = "bare"    // no container
	KindDocker Kind = "docker"  // a Docker/Podman container
	KindLXC    Kind = "lxc"     // an LXC guest, including OCI-derived ones
	KindOCI    Kind = "oci-lxc" // an LXC guest Proxmox built from an OCI image
	KindK8s    Kind = "kubernetes"
)

// Env is what was detected.
type Env struct {
	Kind Kind
	// PVENode is true when this machine is itself a Proxmox VE node, which is
	// the only place pct and pvesh exist.
	PVENode bool
	// HasPCT / HasPVESH report whether those commands are actually callable
	// here. On a node both are true; in a container both are false.
	HasPCT   bool
	HasPVESH bool
	// HasDocker reports whether a usable Docker socket is reachable.
	HasDocker bool
	// NodeName is the PVE node's name when running on one.
	NodeName string
}

// InContainer reports whether Lighthouse is containerised at all.
func (e Env) InContainer() bool { return e.Kind != KindBare }

// CanDriveNodeLocally reports whether Proxmox work can run without ssh.
func (e Env) CanDriveNodeLocally() bool { return e.HasPCT && e.HasPVESH }

// Explain renders the detection for a startup log line, because the most
// common support question is "why did it not find my guests".
func (e Env) Explain() string {
	var b strings.Builder
	b.WriteString("running in ")
	b.WriteString(string(e.Kind))
	if e.PVENode {
		b.WriteString(" on a Proxmox node")
		if e.NodeName != "" {
			b.WriteString(" (" + e.NodeName + ")")
		}
	}
	switch {
	case e.CanDriveNodeLocally():
		b.WriteString("; pct/pvesh available locally")
	case e.InContainer():
		b.WriteString("; pct/pvesh not reachable from inside a container -- " +
			"Proxmox work needs --ssh-host")
	default:
		b.WriteString("; pct/pvesh not found")
	}
	if e.HasDocker {
		b.WriteString("; Docker socket available")
	}
	return b.String()
}

// Detect inspects the current machine.
//
// Every probe is cheap and read-only, and every one of them falls back to a
// safe answer, because a wrong guess here should degrade to "ask the user for
// a flag" rather than to a crash.
func Detect(ctx context.Context, r execx.Runner) Env {
	if r == nil {
		r = execx.Local{}
	}
	e := Env{Kind: detectKind()}

	// pct and pvesh are the real test for node work: a node that has them can
	// be driven locally, and nothing else can, regardless of what else is
	// installed.
	e.HasPCT = onPath(ctx, r, "pct")
	e.HasPVESH = onPath(ctx, r, "pvesh")
	// pveversion is the marker for "this is a PVE node", separate from whether
	// the commands are reachable from *here*.
	e.PVENode = e.HasPVESH || fileExists("/etc/pve/local") || fileExists("/usr/bin/pveversion")
	if e.PVENode {
		e.NodeName = nodeName(ctx, r)
	}
	e.HasDocker = dockerReachable(ctx, r)
	return e
}

// detectKind reads the markers each runtime leaves behind. They are checked in
// order of specificity: an OCI guest is an LXC guest, and a Kubernetes pod is
// a container, so the narrower marker has to win.
func detectKind() Kind {
	// Proxmox writes the source image into the guest when it builds one from
	// an OCI image, which is the only way to tell an OCI guest from a plain
	// LXC from the inside.
	if fileExists("/.oci-image") || fileExists("/usr/share/proxmox-oci") {
		return KindOCI
	}
	// systemd-detect-virt's own marker, present in every LXC guest.
	if v := readFile("/run/systemd/container"); v != "" {
		switch strings.TrimSpace(v) {
		case "lxc", "lxc-libvirt":
			return KindLXC
		case "docker", "podman":
			return KindDocker
		}
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return KindK8s
	}
	// The classic Docker marker. Still the most reliable one for containers
	// that are not running systemd, which is most of them.
	if fileExists("/.dockerenv") {
		return KindDocker
	}
	// LXC sets this for the container's init.
	if os.Getenv("container") == "lxc" {
		return KindLXC
	}
	// cgroup paths name the runtime that created the cgroup. This catches
	// older Docker and unprivileged LXC where no marker file exists, and is
	// checked last because cgroup v2 in a unified hierarchy often shows
	// nothing useful.
	if cg := readFile("/proc/1/cgroup"); cg != "" {
		switch {
		case strings.Contains(cg, "/docker/"), strings.Contains(cg, "docker-"):
			return KindDocker
		case strings.Contains(cg, "/lxc/"), strings.Contains(cg, "lxc.payload"):
			return KindLXC
		case strings.Contains(cg, "kubepods"):
			return KindK8s
		}
	}
	return KindBare
}

func onPath(ctx context.Context, r execx.Runner, cmd string) bool {
	res, err := execx.Script(ctx, r, "command -v "+execx.Quote(cmd)+" >/dev/null 2>&1")
	return err == nil && res.OK()
}

// dockerReachable tests the socket rather than the binary. A bind-mounted
// socket with no client installed is still usable through the Go API, and a
// client with no socket is not usable at all, so the binary tells us nothing.
func dockerReachable(ctx context.Context, r execx.Runner) bool {
	for _, sock := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
		if fileExists(sock) {
			return true
		}
	}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return true
	}
	res, err := execx.Script(ctx, r, "docker info >/dev/null 2>&1")
	return err == nil && res.OK()
}

func nodeName(ctx context.Context, r execx.Runner) string {
	// /etc/pve/local is a symlink to /etc/pve/nodes/<name>, which is the
	// node's own name as PVE knows it -- not necessarily the hostname.
	if target, err := os.Readlink("/etc/pve/local"); err == nil {
		if i := strings.LastIndex(target, "/"); i >= 0 {
			return target[i+1:]
		}
	}
	if res, err := execx.Script(ctx, r, "hostname -s 2>/dev/null"); err == nil && res.OK() {
		return res.Out()
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}
