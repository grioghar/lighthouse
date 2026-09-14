package pkgmgr

import (
	"context"
	"strings"
	"testing"

	"github.com/grioghar/lighthouse/pkg/execx"
)

// The parsers are the part most likely to be quietly wrong, because they read
// human-facing output from tools that pad, indent and add sections. Each case
// below is real output, including the parts that are easy to miscount.

func TestParseAptSimulate(t *testing.T) {
	// A dependency pulled in fresh has no [installed] bracket; a held-back
	// package appears only on a Conf line and must not be counted.
	out := `NOTE: This is only a simulation!
Inst libssl3 [3.0.11-1~deb12u2] (3.0.13-1~deb12u1 Debian-Security:12/stable [amd64])
Inst libcurl4 [7.88.1-10+deb12u5] (7.88.1-10+deb12u7 Debian:12.7/stable [amd64])
Inst new-dependency (1.2.3-1 Debian:12/stable [amd64])
Conf libssl3 (3.0.13-1~deb12u1 Debian-Security:12/stable [amd64])
Conf libcurl4 (7.88.1-10+deb12u7 Debian:12.7/stable [amd64])`

	pkgs := parseAptSimulate(out)
	if len(pkgs) != 3 {
		t.Fatalf("want 3 packages, got %d: %v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "libssl3" || pkgs[0].Installed != "3.0.11-1~deb12u2" {
		t.Errorf("first package parsed as %+v", pkgs[0])
	}
	if pkgs[0].Candidate != "3.0.13-1~deb12u1" {
		t.Errorf("candidate parsed as %q", pkgs[0].Candidate)
	}
	// A brand-new dependency has no installed version, and must not inherit
	// the previous line's.
	if pkgs[2].Name != "new-dependency" || pkgs[2].Installed != "" {
		t.Errorf("new dependency parsed as %+v", pkgs[2])
	}
}

func TestParseAptNoUpgrades(t *testing.T) {
	out := `NOTE: This is only a simulation!
Reading package lists...
Building dependency tree...
0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.`
	if pkgs := parseAptSimulate(out); len(pkgs) != 0 {
		t.Fatalf("clean system should yield no packages, got %v", pkgs)
	}
}

func TestParseDNFCheckUpdate(t *testing.T) {
	// The Obsoleting Packages section lists packages that are NOT upgrades.
	// Counting it is the classic way to over-report on RHEL derivatives.
	out := `Last metadata expiration check: 0:12:31 ago on Sun 14 Sep 2026.

openssl.x86_64                     1:3.0.7-27.el9_4                  baseos
kernel-core.x86_64                 5.14.0-427.33.1.el9_4             baseos

Obsoleting Packages
grub2-tools.x86_64                 1:2.06-27.el9                     baseos
    grub2-tools-minimal.x86_64     1:2.06-12.el9                     @baseos`

	pkgs := parseDNFCheckUpdate(out)
	if len(pkgs) != 2 {
		t.Fatalf("want 2 upgrades (obsoletes excluded), got %d: %v", len(pkgs), pkgs)
	}
	// The .arch suffix is stripped so the name matches what a user installs.
	if pkgs[0].Name != "openssl" || pkgs[0].Candidate != "1:3.0.7-27.el9_4" {
		t.Errorf("parsed as %+v", pkgs[0])
	}
	if pkgs[1].Name != "kernel-core" {
		t.Errorf("second parsed as %+v", pkgs[1])
	}
}

func TestParseAPKVersion(t *testing.T) {
	out := `Installed:                Available:
busybox-1.36.1-r5       < 1.36.1-r7
ca-certificates-bundle-20240226-r0 < 20240705-r0
ssl_client-1.36.1-r5    < 1.36.1-r7`

	pkgs := parseAPKVersion(out)
	if len(pkgs) != 3 {
		t.Fatalf("want 3, got %d: %v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "busybox" || pkgs[0].Installed != "1.36.1-r5" || pkgs[0].Candidate != "1.36.1-r7" {
		t.Errorf("parsed as %+v", pkgs[0])
	}
	// A name containing dashes must survive: only the trailing version and
	// -rN revision are stripped.
	if pkgs[1].Name != "ca-certificates-bundle" {
		t.Errorf("hyphenated name parsed as %q", pkgs[1].Name)
	}
}

func TestParseZypperList(t *testing.T) {
	out := `S | Repository | Name    | Current Version | Available Version | Arch
--+------------+---------+-----------------+-------------------+-------
v | repo-oss   | openssl | 3.0.8-1         | 3.0.9-1           | x86_64
v | repo-oss   | curl    | 8.0.1-1         | 8.4.0-1           | x86_64`

	pkgs := parseZypperList(out)
	if len(pkgs) != 2 {
		t.Fatalf("want 2, got %d: %v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "openssl" || pkgs[0].Installed != "3.0.8-1" || pkgs[0].Candidate != "3.0.9-1" {
		t.Errorf("parsed as %+v", pkgs[0])
	}
}

func TestParsePacmanQu(t *testing.T) {
	out := `openssl 3.1.4-1 -> 3.1.4-2
linux 6.6.8.arch1-1 -> 6.6.10.arch1-1`
	pkgs := parsePacmanQu(out)
	if len(pkgs) != 2 || pkgs[1].Name != "linux" || pkgs[1].Candidate != "6.6.10.arch1-1" {
		t.Fatalf("parsed as %v", pkgs)
	}
}

// fakeRunner replays scripted responses keyed by a substring of the command.
type fakeRunner struct {
	responses map[string]execx.Result
	seen      []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	full := name + " " + strings.Join(args, " ")
	f.seen = append(f.seen, full)
	for key, res := range f.responses {
		if strings.Contains(full, key) {
			return res, nil
		}
	}
	return execx.Result{Code: 127, Stderr: "not found"}, nil
}

func TestDetectUsesOneRoundTrip(t *testing.T) {
	// Probing six managers separately would be six ssh connections per guest;
	// on a node with dozens of guests that dominates the run.
	f := &fakeRunner{responses: map[string]execx.Result{
		"command -v": {Stdout: "apt\n"},
	}}
	b, err := Detect(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "apt" {
		t.Fatalf("detected %s", b.Name)
	}
	if len(f.seen) != 1 {
		t.Fatalf("want 1 command, got %d: %v", len(f.seen), f.seen)
	}
}

func TestDetectNoManagerIsNotAFailure(t *testing.T) {
	// A distroless or scratch container legitimately has no package manager.
	// The caller has to be able to skip it rather than report it broken.
	f := &fakeRunner{responses: map[string]execx.Result{"command -v": {Stdout: "\n"}}}
	if _, err := Detect(context.Background(), f); err != ErrNoManager {
		t.Fatalf("want ErrNoManager, got %v", err)
	}
}

func TestCheckTreatsDNFExit100AsSuccess(t *testing.T) {
	// dnf signals "updates available" with exit 100. Folding that into an
	// error would make the only interesting case look like a broken command.
	f := &fakeRunner{responses: map[string]execx.Result{
		"check-update":     {Code: 100, Stdout: "openssl.x86_64 1:3.0.7-27.el9 baseos\n"},
		"needs-restarting": {Stdout: "unknown\n"},
	}}
	st, err := dnf.Check(context.Background(), f)
	if err != nil {
		t.Fatalf("exit 100 should not be an error: %v", err)
	}
	if st.Count() != 1 {
		t.Fatalf("want 1 pending, got %d", st.Count())
	}
}

func TestCheckSurvivesFailedRefresh(t *testing.T) {
	// One unreachable third-party repo must not suppress the whole report:
	// the index is still usable for everything else.
	f := &fakeRunner{responses: map[string]execx.Result{
		"apt-get update":           {Code: 100, Stderr: "Could not resolve 'ppa.example.com'"},
		"dist-upgrade 2>/dev/null": {Stdout: "Inst libssl3 [1] (2 Debian [amd64])\n"},
		"reboot-required":          {Stdout: "no\n"},
	}}
	st, err := apt.Check(context.Background(), f)
	if err != nil {
		t.Fatalf("failed refresh should not fail the check: %v", err)
	}
	if st.Refreshed {
		t.Error("Refreshed should be false when the refresh failed")
	}
	if st.Count() != 1 {
		t.Fatalf("want 1 pending, got %d", st.Count())
	}
}

func TestRebootUnknownIsNotNo(t *testing.T) {
	// "This distribution cannot tell me" must not be reported as "safe".
	f := &fakeRunner{responses: map[string]execx.Result{
		"check-update":     {Code: 100},
		"needs-restarting": {Stdout: "unknown\n"},
	}}
	st, _ := dnf.Check(context.Background(), f)
	if st.Reboot != Unknown {
		t.Fatalf("want Unknown, got %v", st.Reboot)
	}
	if Unknown.String() != "unknown" || Yes.String() != "yes" || No.String() != "no" {
		t.Error("Tristate strings changed")
	}
}

func TestStatusSummaryCaps(t *testing.T) {
	var st Status
	for _, n := range []string{"zlib", "curl", "openssl", "bash"} {
		st.Pending = append(st.Pending, Package{Name: n})
	}
	// Sorted, so the report is stable run to run.
	if got := st.Summary(2); got != "bash, curl and 2 more" {
		t.Fatalf("got %q", got)
	}
	if got := (Status{}).Summary(2); got != "up to date" {
		t.Fatalf("empty summary got %q", got)
	}
}
