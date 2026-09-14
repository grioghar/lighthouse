package ociguest

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/proxmox"
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
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.Contains(line, k) {
			return execx.Result{Stdout: f.replies[k]}, nil
		}
	}
	return execx.Result{}, nil
}

type resolver struct{ digest string }

func (r resolver) Digest(context.Context, string) (string, error) { return r.digest, nil }

func guestListJSON(tag string) string {
	return `[{"vmid":3136,"name":"plex-oci","status":"running","tags":"` + tag + `"}]`
}

func newProvider(t *testing.T, available string) (*Provider, *fake) {
	t.Helper()
	ref := "docker.io/library/alpine:3.20"
	// The guest carries its reference and installed digest as tags, because
	// Proxmox records no link at all between a guest and its source image.
	tags := proxmox.EncodeRef(ref) + ";" + proxmox.EncodeDigest("sha256:"+strings.Repeat("a", 64))
	f := &fake{replies: map[string]string{"/lxc": guestListJSON(tags)}}
	return &Provider{
		Client:      &proxmox.Client{Run: f, Node: "pve1"},
		Resolver:    resolver{digest: available},
		TemplateDir: "/var/lib/vz/template/cache",
		Storage:     "local",
	}, f
}

func TestDiscoverOnlyReturnsTaggedGuests(t *testing.T) {
	// An untagged guest cannot be tracked: Proxmox stores no guest-to-image
	// link, and the template filename drops the registry and namespace, so the
	// reference is unrecoverable unless we recorded it.
	f := &fake{replies: map[string]string{
		"/lxc": `[{"vmid":3111,"name":"plain-ct","status":"running","tags":"media"}]`,
	}}
	p := &Provider{Client: &proxmox.Client{Run: f, Node: "pve1"}}
	got, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("untagged guest should not be tracked: %+v", got)
	}
}

func TestCheckDetectsDriftWithoutTouchingTheNode(t *testing.T) {
	p, f := newProvider(t, "sha256:"+strings.Repeat("b", 64))
	targets, err := p.Discover(context.Background())
	if err != nil || len(targets) != 1 {
		t.Fatalf("discover: %v %+v", err, targets)
	}
	res := p.Check(context.Background(), targets[0])

	if res.Outcome != update.Outdated {
		t.Fatalf("drift not detected: %+v", res)
	}
	// Detection must be read-only. A check that pulls images or writes to the
	// node is not safe to run on a schedule.
	for _, c := range f.calls {
		if strings.Contains(c, "oci-registry-pull") || strings.Contains(c, "pct ") {
			t.Fatalf("detection mutated the node: %q", c)
		}
	}
}

func TestCheckUsesRecordedDigestNotTheTemplate(t *testing.T) {
	// Reading the template back assumes it is still on the node and still
	// named after the current tag. Templates are large and routinely pruned,
	// so the recorded digest has to be preferred.
	p, f := newProvider(t, "sha256:"+strings.Repeat("a", 64))
	targets, _ := p.Discover(context.Background())
	res := p.Check(context.Background(), targets[0])

	if res.Outcome != update.UpToDate {
		t.Fatalf("got %+v", res)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "tar xOf") {
			t.Fatalf("fell back to the template despite a recorded digest: %q", c)
		}
	}
}

func TestApplyPullsButNeverRebuilds(t *testing.T) {
	// Proxmox squashes layers into one rootfs at creation, so there is no
	// in-place swap. Rebuilding means reproducing the guest's network,
	// mountpoints and resources -- which is where data gets silently lost.
	p, f := newProvider(t, "sha256:"+strings.Repeat("b", 64))
	p.Pull = true
	f.replies["oci-registry-pull"] = "UPID:pve1:0000:task\n"
	f.replies["/tasks/"] = `{"status":"stopped","exitstatus":"OK"}`

	targets, _ := p.Discover(context.Background())
	res := p.Apply(context.Background(), targets[0])

	if !strings.Contains(res.Detail, "pulled") {
		t.Fatalf("should have pulled: %+v", res)
	}
	if !strings.Contains(res.Detail, "manual recreate") {
		t.Errorf("should say a rebuild is still needed: %q", res.Detail)
	}
	// Still outdated: pulling an image does not update the guest.
	if res.Outcome != update.Outdated {
		t.Errorf("pulling alone must not report Updated, got %s", res.Outcome)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "pct destroy") || strings.Contains(c, "pct create") {
			t.Fatalf("apply rebuilt the guest: %q", c)
		}
	}
}

func TestApplyWithPullDisabledChangesNothing(t *testing.T) {
	p, f := newProvider(t, "sha256:"+strings.Repeat("b", 64))
	p.Pull = false
	targets, _ := p.Discover(context.Background())
	res := p.Apply(context.Background(), targets[0])

	if res.Outcome != update.Outdated {
		t.Fatalf("got %s", res.Outcome)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "oci-registry-pull") {
			t.Fatalf("pulled despite Pull being false: %q", c)
		}
	}
}

func TestStoppedGuestIsStillChecked(t *testing.T) {
	// The digest lives on the node, not inside the container, so a stopped
	// guest is checked exactly like a running one. This is the opposite of
	// the package providers, and deliberately so.
	ref := "docker.io/library/alpine:3.20"
	tags := proxmox.EncodeRef(ref) + ";" + proxmox.EncodeDigest("sha256:"+strings.Repeat("a", 64))
	f := &fake{replies: map[string]string{
		"/lxc": `[{"vmid":3136,"name":"off","status":"stopped","tags":"` + tags + `"}]`,
	}}
	p := &Provider{
		Client:   &proxmox.Client{Run: f, Node: "pve1"},
		Resolver: resolver{digest: "sha256:" + strings.Repeat("b", 64)},
	}
	targets, _ := p.Discover(context.Background())
	if len(targets) != 1 {
		t.Fatalf("stopped guest should still be discovered: %+v", targets)
	}
	if res := p.Check(context.Background(), targets[0]); res.Outcome != update.Outdated {
		t.Fatalf("stopped guest should still be checked: %+v", res)
	}
}
