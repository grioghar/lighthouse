package pkgprov

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/pkgmgr"
	"github.com/grioghar/lighthouse/pkg/update"
)

type fake struct {
	replies map[string]string
	calls   []string
}

func (f *fake) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	keys := make([]string, 0, len(f.replies))
	for k := range f.replies {
		keys = append(keys, k)
	}
	// Longest key wins, so a specific reply beats a general one.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.Contains(line, k) {
			return execx.Result{Stdout: f.replies[k]}, nil
		}
	}
	return execx.Result{}, nil
}

func (f *fake) ran(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func aptHost(t *testing.T, replies map[string]string) (*Host, *fake) {
	t.Helper()
	f := &fake{replies: replies}
	h, err := NewHost(f, "pve1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	return h, f
}

func hostTarget() update.Target {
	return update.Target{ID: "host:pve1", Name: "pve1", Runtime: "host"}
}

func TestHostCheckNeverInstalls(t *testing.T) {
	// The safety property that makes a scheduled run against a hypervisor
	// acceptable: a check refreshes the index and reads, nothing more.
	h, f := aptHost(t, map[string]string{
		"command -v apt-get": "apt\n",
		"dist-upgrade 2>/dev/null": "Inst libssl3 [1.0] (1.1 Debian [amd64])\n" +
			"Inst curl [7.0] (7.1 Debian [amd64])\n",
		"reboot-required": "no\n",
	})
	res := h.Check(context.Background(), hostTarget())

	if res.Outcome != update.Outdated || res.Pending != 2 {
		t.Fatalf("got %+v", res)
	}
	if f.ran("apt-get -y") || f.ran("dist-upgrade\n") {
		t.Fatalf("check performed an install: %v", f.calls)
	}
}

func TestHostReportsRebootAndNeverPerformsIt(t *testing.T) {
	// On a node running dozens of guests, rebooting is never something a
	// scheduled run should decide to do.
	h, f := aptHost(t, map[string]string{
		"command -v apt-get":       "apt\n",
		"dist-upgrade 2>/dev/null": "Inst libc6 [1] (2 Debian [amd64])\n",
		"reboot-required":          "yes\n",
	})
	res := h.Check(context.Background(), hostTarget())

	if res.Reboot != pkgmgr.Yes {
		t.Fatalf("reboot not reported: %+v", res)
	}
	if !strings.Contains(res.Detail, "REBOOT REQUIRED") {
		t.Errorf("detail should state it loudly: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "not performed") {
		t.Errorf("detail should say it was not done: %q", res.Detail)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "reboot") && !strings.Contains(c, "reboot-required") {
			t.Fatalf("something tried to reboot: %q", c)
		}
	}
}

func TestHostCallsOutProxmoxKernelUpgrades(t *testing.T) {
	// Installing a kernel changes nothing until the node restarts. Reporting
	// "upgraded, now up to date" without saying so is actively misleading.
	h, _ := aptHost(t, map[string]string{
		"command -v apt-get": "apt\n",
		"dist-upgrade 2>/dev/null": "Inst proxmox-kernel-6.8.12-4-pve [6.8.12-3] (6.8.12-4 Proxmox [amd64])\n" +
			"Inst zlib1g [1] (2 Debian [amd64])\n",
		// Deliberately unknown: the marker file has not appeared yet.
		"reboot-required": "unknown\n",
	})
	res := h.Check(context.Background(), hostTarget())

	if !strings.Contains(res.Detail, "proxmox-kernel-6.8.12-4-pve") {
		t.Fatalf("kernel not named: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "no effect until the node is rebooted") {
		t.Errorf("consequence not stated: %q", res.Detail)
	}
}

func TestHostApplyInfersRebootFromKernel(t *testing.T) {
	h, _ := aptHost(t, map[string]string{
		"command -v apt-get":       "apt\n",
		"dist-upgrade 2>/dev/null": "Inst pve-kernel-6.5 [1] (2 Proxmox [amd64])\n",
		"reboot-required":          "unknown\n",
	})
	res := h.Apply(context.Background(), hostTarget())

	// The post-upgrade re-check still lists the kernel here (the fake always
	// returns the same thing), which is enough to prove the inference fires.
	if res.Reboot != pkgmgr.Yes {
		t.Fatalf("a kernel upgrade must imply a reboot even when the marker is absent: %+v", res)
	}
}

func TestHostIsSerial(t *testing.T) {
	// dpkg takes a machine-wide lock.
	h, _ := aptHost(t, nil)
	if !h.Serial() {
		t.Fatal("host provider must be serial")
	}
}

func TestHostWithNoPackageManagerIsSkippedNotFailed(t *testing.T) {
	// A host we cannot introspect is not a broken host.
	h, _ := aptHost(t, map[string]string{"command -v apt-get": "\n"})
	res := h.Check(context.Background(), hostTarget())
	if res.Outcome != update.Skipped {
		t.Fatalf("got %+v, want skipped", res)
	}
	if res.Err != nil {
		t.Errorf("should carry no error: %v", res.Err)
	}
}

func TestHostApplyReportsHeldBackPackages(t *testing.T) {
	// apt exits zero while holding packages back (phased updates, holds).
	// Reporting that as "done" hides the fact that nothing moved.
	h, _ := aptHost(t, map[string]string{
		"command -v apt-get":       "apt\n",
		"dist-upgrade 2>/dev/null": "Inst somepkg [1] (2 Debian [amd64])\n",
		"reboot-required":          "no\n",
	})
	res := h.Apply(context.Background(), hostTarget())

	if res.Outcome != update.Updated {
		t.Fatalf("got %s", res.Outcome)
	}
	if !strings.Contains(res.Detail, "held back") {
		t.Fatalf("held-back packages not reported: %q", res.Detail)
	}
}

func TestContainersWarnAboutEphemeralFilesystem(t *testing.T) {
	// An in-container upgrade is discarded the moment the container is
	// recreated from its image. Saying so on every actionable line is the
	// point -- this is the detail that makes the result misleading if missed.
	f := &fake{replies: map[string]string{
		"command -v apt-get":       "apt\n",
		"dist-upgrade 2>/dev/null": "Inst openssl [1] (2 Debian [amd64])\n",
		"reboot-required":          "no\n",
	}}
	c, err := NewContainers(f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := c.Check(context.Background(), update.Target{
		ID: "docker:plex", Name: "plex", Labels: map[string]string{"container": "plex"},
	})
	if res.Outcome != update.Outdated {
		t.Fatalf("got %s", res.Outcome)
	}
	if !strings.Contains(res.Detail, "lost when the container is recreated") {
		t.Fatalf("ephemerality not stated: %q", res.Detail)
	}
}

func TestUnknownManagerIsRejectedAtConstruction(t *testing.T) {
	// Better to fail at startup naming the valid values than to run a
	// schedule that silently detects something else.
	if _, err := NewHost(&fake{}, "pve1", Options{Manager: "nope"}); err == nil {
		t.Fatal("unknown manager should be rejected")
	} else if !strings.Contains(err.Error(), "apt") {
		t.Errorf("error should list what is valid: %v", err)
	}
}
