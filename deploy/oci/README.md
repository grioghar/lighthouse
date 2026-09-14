# Running the Lighthouse agent as a Proxmox OCI guest

This is the deployment that closes the loop: Lighthouse lives in the same kind
of container it watches.

## Build and publish the image

```sh
docker build -f deploy/oci/Dockerfile -t docker.io/<you>/lighthouse-agent:0.1.0 .
docker push docker.io/<you>/lighthouse-agent:0.1.0
```

Use a real version tag, not `latest`. Proxmox derives the template filename
from the last path element and the tag — `lighthouse-agent_0.1.0.tar` — so two
images that differ only by registry collide, and `latest` makes the installed
digest impossible to reason about.

## Pull it on the node

```sh
pvesh create /nodes/<node>/storage/local/oci-registry-pull \
  --reference docker.io/<you>/lighthouse-agent:0.1.0
```

That returns a UPID and keeps working in the background. It writes to the
**template cache** — `/var/lib/vz/template/cache/` — not the "import" content
directory the storage configuration implies. Wait for it before creating the
guest, or you will find an empty store and conclude the pull failed:

```sh
pvesh get /nodes/<node>/tasks/<UPID>/status
```

## Create the guest

```sh
pct create 3140 local:vztmpl/lighthouse-agent_0.1.0.tar \
  --hostname lighthouse \
  --memory 512 --cores 1 \
  --net0 name=eth0,bridge=vmbr0,ip=dhcp \
  --unprivileged 1 \
  --features nesting=1
```

## Give it access to the node

The guest cannot run `pct` or `pvesh`. They exist only on the node and cannot
be installed anywhere else, so node and guest work goes over ssh — only the
process moves, not the commands.

On the node:

```sh
ssh-keygen -t ed25519 -f /root/.ssh/lighthouse -N ''
cat /root/.ssh/lighthouse.pub >> /root/.ssh/authorized_keys
pct push 3140 /root/.ssh/lighthouse /etc/lighthouse/id_ed25519 --perms 0600
```

The key must have no passphrase. The agent runs ssh in `BatchMode`, which
refuses to prompt — there is nobody to answer it — so a passphrase-protected
key fails rather than hanging.

Seed the host key too, or the first connection fails verification:

```sh
pct exec 3140 -- sh -c 'mkdir -p /root/.ssh && ssh-keyscan <node-ip> >> /root/.ssh/known_hosts'
```

## Configure and start

Set these on the guest (`pct set 3140 --...` or in the guest's environment):

| Variable | Meaning |
|---|---|
| `LIGHTHOUSE_NODE` | Proxmox node name. Enables the Proxmox providers. |
| `LIGHTHOUSE_SSH_HOST` | `root@<node-ip>`. Required whenever `LIGHTHOUSE_NODE` is set. |
| `LIGHTHOUSE_SSH_KEY` | Defaults to `/etc/lighthouse/id_ed25519`. |
| `LIGHTHOUSE_SCHEDULE` | Cron expression. Defaults to `0 0 4 * * *`. |
| `LIGHTHOUSE_APPLY` | `true` installs updates. Anything else reports only. |
| `LIGHTHOUSE_EXTRA_ARGS` | Passed through verbatim. |
| `TZ` | The zone the schedule is read in. |

Check what it can actually reach before trusting a scheduled run:

```sh
pct exec 3140 -- lighthouse providers
```

## Updating the agent itself

There is no in-place update. Proxmox squashed the image into the guest's
rootfs at creation, so the image no longer exists as a thing to swap. Pull the
new tag and recreate the guest.

This is the same constraint the `oci-images` provider reports on, which is why
that provider pulls and then stops: rebuilding a guest means reproducing its
network, mountpoints and resources, and getting that subtly wrong loses data
silently. For this guest that is cheap — it holds no state.
