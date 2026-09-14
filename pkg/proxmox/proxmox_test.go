package proxmox

import (
	"archive/tar"
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/grioghar/lighthouse/pkg/execx"
)

func TestParseModes(t *testing.T) {
	m, err := ParseModes("packages")
	if err != nil || !m.Enabled(ModePackages) || m.Enabled(ModeOCI) {
		t.Fatalf("packages only: %v %v", m, err)
	}
	// recreate implies oci: you cannot choose what to rebuild without comparing.
	m, err = ParseModes("recreate")
	if err != nil || !m.Enabled(ModeOCI) || !m.Enabled(ModeRecreate) {
		t.Fatalf("recreate should imply oci: %v %v", m, err)
	}
	if m, err := ParseModes("off"); err != nil || len(m) != 0 {
		t.Fatalf("off should enable nothing: %v %v", m, err)
	}
	if _, err := ParseModes("nope"); err == nil {
		t.Fatal("unknown mode should error")
	}
}

func TestRefTagRoundTrip(t *testing.T) {
	// Proxmox lowercases tags and restricts the charset, so a raw reference
	// cannot survive; the encoding has to.
	for _, ref := range []string{
		"docker.io/library/alpine:3.20",
		"lscr.io/linuxserver/plex:latest",
		"ghcr.io/Some/Mixed-Case:v1.2.3",
	} {
		tag := EncodeRef(ref)
		if strings.ToLower(tag) != tag {
			t.Fatalf("tag %q is not lowercase", tag)
		}
		got, ok := DecodeRef(strings.ToLower(tag))
		if !ok || got != ref {
			t.Fatalf("round trip of %q gave %q (ok=%v)", ref, got, ok)
		}
	}
	if _, ok := DecodeRef("someone-elses-tag"); ok {
		t.Fatal("foreign tag should not decode")
	}
}

func TestTemplateFile(t *testing.T) {
	cases := map[string]string{
		"docker.io/library/alpine:3.20":   "alpine_3.20.tar",
		"lscr.io/linuxserver/plex:latest": "plex_latest.tar",
		"alpine":                          "alpine_latest.tar",
		"registry:5000/team/app:1.0":      "app_1.0.tar",
	}
	for ref, want := range cases {
		if got := TemplateFile(ref); got != want {
			t.Errorf("TemplateFile(%q) = %q, want %q", ref, got, want)
		}
	}
}

func ociTar(t *testing.T, digest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := `{"schemaVersion":2,"manifests":[{"digest":"` + digest + `","size":1023}]}`
	for _, f := range []struct{ name, content string }{
		{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`},
		{"index.json", body},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Size: int64(len(f.content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestInstalledDigest(t *testing.T) {
	want := "sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e"
	got, err := InstalledDigest(bytes.NewReader(ociTar(t, want)))
	if err != nil || got != want {
		t.Fatalf("got %q err %v", got, err)
	}
	// A plain rootfs tar is not an OCI layout and must be rejected, not guessed at.
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	tw.WriteHeader(&tar.Header{Name: "etc/hostname", Size: 0, Mode: 0o644})
	tw.Close()
	if _, err := InstalledDigest(bytes.NewReader(plain.Bytes())); err == nil {
		t.Fatal("non-OCI tar should error")
	}
}

// fakeRunner replays canned output keyed by a substring of the command line.
type fakeRunner struct {
	replies map[string]string
	calls   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	// Longest key first, so a specific reply wins over a general one: the
	// package probe and the package listing both arrive as "pct exec".
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

type fakeResolver struct{ digest string }

func (f fakeResolver) Digest(context.Context, string) (string, error) { return f.digest, nil }

func TestCheckOCIDetectsDrift(t *testing.T) {
	ref := "docker.io/library/alpine:3.20"
	guests := `[{"vmid":101,"name":"app","status":"running","tags":"` + EncodeRef(ref) + `"}]`
	r := &fakeRunner{replies: map[string]string{
		"/lxc":    guests,
		"tar xOf": `{"schemaVersion":2,"manifests":[{"digest":"sha256:old"}]}`,
	}}
	u := &Updater{
		Client:      &Client{Run: r, Node: "pve"},
		Modes:       Modes{ModeOCI: true},
		Resolver:    fakeResolver{digest: "sha256:new"},
		TemplateDir: "/var/lib/vz/template/cache",
	}
	res, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d", len(res))
	}
	got := res[0]
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if !got.Outdated || got.Installed != "sha256:old" || got.Available != "sha256:new" {
		t.Fatalf("drift not detected: %+v", got)
	}
	if got.Guest.Image != ref {
		t.Fatalf("reference not recovered from tag: %q", got.Guest.Image)
	}
	// Detection alone must not touch the guest.
	for _, c := range r.calls {
		if strings.Contains(c, "oci-registry-pull") || strings.Contains(c, "pct ") {
			t.Fatalf("detection mode mutated the node: %q", c)
		}
	}
}

func TestUnmanagedGuestGoesToPackages(t *testing.T) {
	// The package half is no longer apt-specific: the guest is probed for
	// whichever manager it has, and an Alpine guest is handled by the same
	// code path as a Debian one.
	guests := `[{"vmid":102,"name":"alpine-ct","status":"running","tags":""}]`
	r := &fakeRunner{replies: map[string]string{
		"/lxc": guests,
		// The one-round-trip probe: the guest answers with its manager.
		"command -v apt-get": "apk\n",
		"apk version -l":     "Installed:      Available:\nbusybox-1.36.1-r5 < 1.36.1-r7\nzlib-1.3-r0 < 1.3.1-r0\n",
	}}
	u := &Updater{
		Client: &Client{Run: r, Node: "pve"},
		Modes:  Modes{ModePackages: true},
		DryRun: true,
	}
	res, _ := u.Check(context.Background())
	if len(res) != 1 || res[0].Kind != ModePackages {
		t.Fatalf("expected a packages result, got %+v", res)
	}
	if res[0].Manager != "apk" {
		t.Fatalf("manager detected as %q, want apk", res[0].Manager)
	}
	if res[0].Packages != 2 || !res[0].Outdated {
		t.Fatalf("package count not parsed: %+v", res[0])
	}
	if !strings.Contains(res[0].Action, "would upgrade") {
		t.Fatalf("dry run should not upgrade: %q", res[0].Action)
	}
	// A dry run must not reach the upgrade command.
	for _, c := range r.calls {
		if strings.Contains(c, "apk upgrade") {
			t.Fatalf("dry run ran an upgrade: %q", c)
		}
	}
}

func TestStoppedGuestIsSkippedNotFailed(t *testing.T) {
	// pct exec cannot enter a stopped guest. Reporting that as an error makes
	// a normal fleet look broken -- half a homelab is stopped at any moment.
	guests := `[{"vmid":103,"name":"off-ct","status":"stopped","tags":""}]`
	r := &fakeRunner{replies: map[string]string{"/lxc": guests}}
	u := &Updater{
		Client: &Client{Run: r, Node: "pve"},
		Modes:  Modes{ModePackages: true},
		DryRun: true,
	}
	res, _ := u.Check(context.Background())
	if len(res) != 1 || res[0].Err != nil || res[0].Outdated {
		t.Fatalf("stopped guest should be a clean skip: %+v", res)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "pct exec") {
			t.Fatalf("stopped guest was entered: %q", c)
		}
	}
}

func TestPullRejectsDigestReference(t *testing.T) {
	// The Proxmox reference pattern accepts a tag only; catching it here gives
	// a clear error instead of a regex rejection from the API.
	c := &Client{Run: &fakeRunner{}, Node: "pve"}
	if _, err := c.PullOCI(context.Background(), "local", "alpine@sha256:abc"); err == nil {
		t.Fatal("digest reference should be rejected")
	}
}

func TestPlatformDigestPicksTheArchNotTheIndex(t *testing.T) {
	// A registry HEAD on a multi-arch tag reports the index digest, but Proxmox
	// stores the platform manifest's. Comparing those marks every multi-arch
	// guest permanently outdated, so the index must be resolved down first.
	body := []byte(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
	  {"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}},
	  {"digest":"sha256:unknown","platform":{"os":"unknown","architecture":"unknown"}},
	  {"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}}]}`)
	got, err := PlatformDigest(body, "sha256:theindex", "linux", "amd64")
	if err != nil || got != "sha256:amd" {
		t.Fatalf("got %q err %v, want sha256:amd", got, err)
	}
	if got, _ := PlatformDigest(body, "sha256:theindex", "linux", "arm64"); got != "sha256:arm" {
		t.Fatalf("arm64 lookup gave %q", got)
	}
	if _, err := PlatformDigest(body, "sha256:theindex", "linux", "riscv64"); err == nil {
		t.Fatal("missing platform should error, not silently pick one")
	}
	// Single-platform manifest: fall back to the tag's own digest.
	single := []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{}}`)
	got, err = PlatformDigest(single, "sha256:plain", "linux", "amd64")
	if err != nil || got != "sha256:plain" {
		t.Fatalf("single-platform gave %q err %v", got, err)
	}
}
