#!/bin/sh
# Install the Lighthouse agent into an LXC guest, an OCI guest, or a bare host
# (including a Proxmox node).
#
# Deliberately POSIX sh with no dependencies beyond coreutils and systemd: the
# targets include Alpine-based OCI guests with no bash.
#
# Usage:
#   ./install.sh                 build from this checkout and install
#   ./install.sh /path/to/binary install an already-built binary
set -eu

BIN="${1:-}"
PREFIX="${PREFIX:-/usr/local/bin}"
CONFDIR="${CONFDIR:-/etc/lighthouse}"
UNITDIR="${UNITDIR:-/etc/systemd/system}"
SRCDIR="$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)"

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root (it writes to $PREFIX and $UNITDIR)" >&2
    exit 1
fi

if [ -z "$BIN" ]; then
    command -v go >/dev/null 2>&1 || {
        echo "No binary given and no Go toolchain found." >&2
        echo "Either install Go, or build elsewhere and pass the binary:" >&2
        echo "  GOOS=linux GOARCH=amd64 go build -o lighthouse . && ./install.sh ./lighthouse" >&2
        exit 1
    }
    echo "Building from $SRCDIR ..."
    BIN="$(mktemp -d)/lighthouse"
    ( cd "$SRCDIR" && CGO_ENABLED=0 go build -o "$BIN" . )
fi

echo "Installing binary to $PREFIX/lighthouse"
install -d "$PREFIX"
install -m 0755 "$BIN" "$PREFIX/lighthouse"

install -d -m 0750 "$CONFDIR"
if [ ! -f "$CONFDIR/agent.env" ]; then
    install -m 0640 "$SRCDIR/deploy/lxc/agent.env.example" "$CONFDIR/agent.env"
    echo "Wrote $CONFDIR/agent.env -- edit it before starting the service."
else
    echo "Keeping existing $CONFDIR/agent.env"
fi

# An OCI guest often has no systemd at all; the binary is still useful there,
# driven by whatever supervises PID 1.
if [ ! -d "$UNITDIR" ] || ! command -v systemctl >/dev/null 2>&1; then
    echo
    echo "No systemd here, so no service was installed."
    echo "Run the agent directly, e.g.:"
    echo "  $PREFIX/lighthouse agent --schedule='0 0 4 * * *' --host-packages"
    exit 0
fi

echo "Installing systemd unit to $UNITDIR"
install -m 0644 "$SRCDIR/deploy/lxc/lighthouse-agent.service" "$UNITDIR/lighthouse-agent.service"
systemctl daemon-reload

echo
echo "Installed. Next:"
echo "  1. Edit $CONFDIR/agent.env"
echo "  2. Check what this host can reach:  $PREFIX/lighthouse providers"
echo "  3. Try one run by hand:             $PREFIX/lighthouse agent --host-packages"
echo "  4. Enable it:                       systemctl enable --now lighthouse-agent"
echo
echo "Nothing is changed until you add --apply. The node is never rebooted."
