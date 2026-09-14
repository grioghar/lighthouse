package pkgmgr

import "strings"

// One entry per distribution family. Adding support for another is adding a
// value here and appending it to Backends -- no caller changes.
//
// Two conventions hold throughout:
//
//   - Every command is non-interactive and answers no question. An unattended
//     run that stops on a config-file prompt is worse than one that does
//     nothing, because it holds a lock and blocks the next run too.
//   - List never installs anything. Check is safe to run on a schedule against
//     a production guest; only Upgrade changes installed software.

// apt covers Debian, Ubuntu, and the Proxmox host itself.
var apt = &Backend{
	Name:   "apt",
	Binary: "apt-get",
	// -qq keeps the transfer log out of the output; the index still updates,
	// and the W: lines below are still printed to stderr.
	RefreshCmd: "apt-get update -qq",
	// apt exits 0 with an unreachable repository, so the warning text is the
	// only signal that the index is now stale.
	RefreshWarnings: []string{
		"Some index files failed to download",
		"Failed to fetch",
	},
	// A simulated dist-upgrade is the only apt output that reports what would
	// actually be installed, including packages pulled in by new dependencies.
	// `apt list --upgradable` misses those, so it undercounts.
	ListCmd: "apt-get -s -o Debug::NoLocking=1 dist-upgrade 2>/dev/null",
	// --force-confold keeps existing config files. The alternative is a prompt
	// (which hangs) or silently overwriting a hand-edited config (which is
	// worse than not upgrading).
	UpgradeCmd: "DEBIAN_FRONTEND=noninteractive apt-get -y " +
		"-o Dpkg::Options::=--force-confold -o Dpkg::Options::=--force-confdef " +
		"dist-upgrade",
	RebootCmd: "test -f /var/run/reboot-required && echo yes || echo no",
	Parse:     parseAptSimulate,
}

// dnf covers Fedora, RHEL 8+, Rocky and Alma.
var dnf = &Backend{
	Name:   "dnf",
	Binary: "dnf",
	// check-update refreshes metadata itself, so there is no separate step.
	ListCmd: "dnf -q check-update 2>/dev/null",
	// 100 is check-update's way of saying "updates are available". Treating it
	// as an error would make the only interesting case look like a failure.
	ListOKCodes: []int{100},
	UpgradeCmd:  "dnf -y --refresh upgrade",
	// needs-restarting ships in dnf-utils and is often absent; saying "unknown"
	// is honest, whereas the naive `&& echo no || echo yes` would report a
	// missing tool as a required reboot.
	RebootCmd: "if command -v needs-restarting >/dev/null 2>&1; then " +
		"needs-restarting -r >/dev/null 2>&1 && echo no || echo yes; else echo unknown; fi",
	Parse: parseDNFCheckUpdate,
}

// yum covers CentOS 7 and RHEL 7, whose output format matches dnf's.
var yum = &Backend{
	Name:        "yum",
	Binary:      "yum",
	ListCmd:     "yum -q check-update 2>/dev/null",
	ListOKCodes: []int{100},
	UpgradeCmd:  "yum -y update",
	RebootCmd: "if command -v needs-restarting >/dev/null 2>&1; then " +
		"needs-restarting -r >/dev/null 2>&1 && echo no || echo yes; else echo unknown; fi",
	Parse: parseDNFCheckUpdate,
}

// apk covers Alpine, which is most OCI-derived guests.
var apk = &Backend{
	Name:       "apk",
	Binary:     "apk",
	RefreshCmd: "apk update -q",
	// `apk version -l '<'` lists packages whose installed version sorts below
	// what the repository offers -- i.e. exactly the upgradable set.
	ListCmd:    "apk version -l '<' 2>/dev/null",
	UpgradeCmd: "apk upgrade --no-cache",
	// Alpine ships no reboot-required marker, and in a container the question
	// is meaningless anyway.
	Parse: parseAPKVersion,
}

// zypper covers openSUSE and SLES.
var zypper = &Backend{
	Name:       "zypper",
	Binary:     "zypper",
	RefreshCmd: "zypper -n -q refresh",
	ListCmd:    "zypper -n -q list-updates 2>/dev/null",
	UpgradeCmd: "zypper -n -q update --auto-agree-with-licenses",
	// zypper exits 102 when a reboot is required and 0 when it is not.
	RebootCmd: "if zypper -n needs-rebooting >/dev/null 2>&1; then echo no; else echo yes; fi",
	Parse:     parseZypperList,
}

// pacman covers Arch and its derivatives.
var pacman = &Backend{
	Name:   "pacman",
	Binary: "pacman",
	// -Sy alone leaves a partially-updated index, which is the documented way
	// to break an Arch system. It is acceptable only because Upgrade always
	// does a full -Syu, never an isolated -S.
	RefreshCmd:      "pacman -Sy --noconfirm",
	RefreshWarnings: []string{"failed retrieving file", "failed to update"},
	ListCmd:         "pacman -Qu 2>/dev/null",
	UpgradeCmd:      "pacman -Syu --noconfirm",
	// Arch has no marker file; compare the running kernel against the
	// installed one, which is the check that actually matters.
	RebootCmd: "inst=$(pacman -Q linux 2>/dev/null | awk '{print $2}'); " +
		"run=$(uname -r); case \"$run\" in \"\") echo unknown;; *) " +
		"case \"$inst\" in \"\") echo unknown;; *) " +
		"echo \"$run\" | grep -q \"$(echo \"$inst\" | cut -d- -f1)\" && echo no || echo yes;; esac;; esac",
	Parse: parsePacmanQu,
}

// parseAptSimulate reads `apt-get -s dist-upgrade`, whose relevant lines are:
//
//	Inst libssl3 [3.0.11-1~deb12u2] (3.0.13-1~deb12u1 Debian:12/stable [amd64])
//
// Conf lines describe the same packages and are ignored.
func parseAptSimulate(stdout string) []Package {
	var out []Package
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		p := Package{Name: fields[1]}
		// The bracketed field is the installed version; a package being newly
		// pulled in as a dependency has no bracket at all.
		if len(fields) > 2 && strings.HasPrefix(fields[2], "[") {
			p.Installed = strings.Trim(fields[2], "[]")
		}
		if i := strings.Index(line, "("); i >= 0 {
			if cand := strings.Fields(line[i+1:]); len(cand) > 0 {
				p.Candidate = strings.TrimLeft(cand[0], "(")
			}
		}
		out = append(out, p)
	}
	return out
}

// parseDNFCheckUpdate reads `dnf check-update`, whose lines are:
//
//	openssl.x86_64    1:3.0.7-27.el9    baseos
//
// A blank line separates the upgradable set from the "Obsoleting Packages"
// section, which must not be counted, and dnf may emit a leading blank line
// before the table -- so the split is on the first blank line *after* content.
func parseDNFCheckUpdate(stdout string) []Package {
	var out []Package
	seenContent := false
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if seenContent {
				break
			}
			continue
		}
		fields := strings.Fields(trimmed)
		// A continuation line is indented and has fewer columns; a header such
		// as "Last metadata expiration check" has no version column.
		if len(fields) < 3 || strings.HasPrefix(line, " ") {
			continue
		}
		if strings.HasPrefix(trimmed, "Obsoleting") || strings.HasPrefix(trimmed, "Last metadata") {
			continue
		}
		seenContent = true
		name := fields[0]
		// Strip the .arch suffix so names match what the user would install.
		if i := strings.LastIndex(name, "."); i > 0 {
			name = name[:i]
		}
		out = append(out, Package{Name: name, Candidate: fields[1]})
	}
	return out
}

// parseAPKVersion reads `apk version -l '<'`, whose lines are:
//
//	busybox-1.36.1-r5     <  1.36.1-r7
//
// preceded by an "Installed: ... Available:" header.
func parseAPKVersion(stdout string) []Package {
	var out []Package
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Installed:") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 3 || fields[1] != "<" {
			continue
		}
		// apk reports name-version as one token; the name is everything before
		// the last two dash-separated fields (version and -rN revision).
		name, installed := splitAPKName(fields[0])
		out = append(out, Package{Name: name, Installed: installed, Candidate: fields[2]})
	}
	return out
}

func splitAPKName(token string) (name, version string) {
	parts := strings.Split(token, "-")
	if len(parts) < 3 {
		return token, ""
	}
	// The last part is the -rN revision and the one before it the version.
	return strings.Join(parts[:len(parts)-2], "-"),
		strings.Join(parts[len(parts)-2:], "-")
}

// parseZypperList reads `zypper list-updates`, whose table rows are:
//
//	v | repo | openssl | 3.0.8-1 | 3.0.9-1 | x86_64
func parseZypperList(stdout string) []Package {
	var out []Package
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "v |") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 5 {
			continue
		}
		out = append(out, Package{
			Name:      strings.TrimSpace(cols[2]),
			Installed: strings.TrimSpace(cols[3]),
			Candidate: strings.TrimSpace(cols[4]),
		})
	}
	return out
}

// parsePacmanQu reads `pacman -Qu`, whose lines are:
//
//	openssl 3.1.4-1 -> 3.1.4-2
func parsePacmanQu(stdout string) []Package {
	var out []Package
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 || fields[2] != "->" {
			continue
		}
		out = append(out, Package{Name: fields[0], Installed: fields[1], Candidate: fields[3]})
	}
	return out
}
