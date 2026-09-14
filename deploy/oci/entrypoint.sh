#!/bin/sh
# PID 1 for the Lighthouse OCI guest.
#
# Configuration comes from the environment because a Proxmox OCI guest has no
# convenient way to pass a command line -- the image's entrypoint is what runs,
# and changing it means rebuilding the image.
set -eu

: "${LIGHTHOUSE_SCHEDULE:=0 0 4 * * *}"
: "${LIGHTHOUSE_NODE:=}"
: "${LIGHTHOUSE_SSH_HOST:=}"
: "${LIGHTHOUSE_SSH_KEY:=/etc/lighthouse/id_ed25519}"
: "${LIGHTHOUSE_APPLY:=false}"
: "${LIGHTHOUSE_EXTRA_ARGS:=}"

set -- agent --schedule="$LIGHTHOUSE_SCHEDULE"

if [ -n "$LIGHTHOUSE_NODE" ]; then
    set -- "$@" --node="$LIGHTHOUSE_NODE"
    # pct and pvesh cannot exist inside a guest, so node work must go over ssh.
    # Saying so here beats every guest failing with "pct: command not found".
    if [ -z "$LIGHTHOUSE_SSH_HOST" ]; then
        echo "LIGHTHOUSE_NODE is set but LIGHTHOUSE_SSH_HOST is not." >&2
        echo "This guest cannot run pct or pvesh -- they exist only on the node." >&2
        echo "Set LIGHTHOUSE_SSH_HOST=root@<node-ip> and mount a key." >&2
        exit 1
    fi
    set -- "$@" --ssh-host="$LIGHTHOUSE_SSH_HOST"
    [ -f "$LIGHTHOUSE_SSH_KEY" ] && set -- "$@" --ssh-key="$LIGHTHOUSE_SSH_KEY"
else
    set -- "$@" --host-packages
fi

[ "$LIGHTHOUSE_APPLY" = "true" ] && set -- "$@" --apply

# Word-splitting is intended here: extra args arrive as one string.
# shellcheck disable=SC2086
exec /usr/local/bin/lighthouse "$@" $LIGHTHOUSE_EXTRA_ARGS
