# Fleet updates

Lighthouse began as a Docker image updater, where "update" meant one thing:
pull a newer image and replace the container. A homelab needs at least four,
and they share almost nothing:

| What | What "update" means |
|---|---|
| Docker container | Replace it with one built from a newer image |
| OCI-derived LXC | Compare the installed manifest digest to the registry |
| Conventional LXC | Run the distribution's package manager inside it |
| Proxmox node | Run apt on the hypervisor — and never reboot it |

Each is a **provider**. The engine knows only the interface, so adding a fifth
is a new file rather than an edit to a dispatcher.

```bash
lighthouse agent --node pve1
```

## The safety property

**A check never installs, removes, restarts or recreates anything.** Dry-run is
not a flag each provider has to remember to honour — it is `Apply` never being
called. Nothing changes unless you pass `--apply`.

The one exception is that a check refreshes the package index, which is a
write. Without it, a check reports against a stale index and is not worth
having. Nothing installed changes.

## Providers

| Provider | Updates | Requires |
|---|---|---|
| `docker-images` | Containers whose image has a newer version | A reachable Docker socket |
| `oci-images` | Proxmox guests built from an OCI image | `--node`, plus `pct`/`pvesh` access |
| `lxc-packages` | Packages inside Proxmox LXC guests | `--node`, plus `pct`/`pvesh` access |
| `host-packages` | Packages on the host or Proxmox node itself | A supported package manager |
| `docker-packages` | Packages inside running containers | Opt-in — see below |

Providers are enabled automatically from what is configured and reachable.
To see what this machine can actually reach, and why:

```bash
lighthouse providers
```

Run that first when a provider is not doing what you expect. It distinguishes
"not configured" from "configured but unreachable", which look identical in a
run's output.

## Package managers

`lxc-packages`, `host-packages` and `docker-packages` all detect the target's
package manager rather than assuming apt:

`apt` · `dnf` · `yum` · `apk` · `zypper` · `pacman`

Detection is one round trip, not one per candidate — on a node with 46 guests,
probing six managers separately would make connection setup the dominant cost
of a run. Force one with `--package-manager` if your fleet is homogeneous.

Adding a distribution is one table entry in `pkg/pkgmgr/backends.go`.

## The Proxmox node

The node is the most conservative target in the tree, because the blast radius
is the whole fleet:

- **It is never rebooted.** A pending reboot is reported loudly and left for a
  human to schedule. On a hypervisor that means every guest going down.
- **Kernel upgrades are called out by name.** Installing `proxmox-kernel-*`
  changes nothing until the node restarts, and a report that said "upgraded,
  now up to date" would be actively misleading.
- **Upgrades are serialised.** `dpkg` takes a machine-wide lock; concurrency
  against one host does not go faster, it dies on the lock.

## Where Lighthouse runs vs. where commands land

These are separate, and conflating them is the most common misconfiguration.

`pct` and `pvesh` exist **only on a Proxmox node** and cannot be installed
anywhere else. So when Lighthouse runs in a container — the normal deployment —
the process moves but the commands do not. They go over ssh:

```bash
lighthouse agent --node pve1 --ssh-host root@192.168.1.10 --ssh-key /etc/lighthouse/id_ed25519
```

Passing `--node` without `--ssh-host` from inside a container is refused at
startup with that explanation, rather than failing once per guest with
`pct: command not found`.

The key must have no passphrase: ssh runs in `BatchMode`, which refuses to
prompt, because there is nobody to answer it. A fresh container also has an
empty `known_hosts`, so seed it:

```bash
ssh-keyscan <node-ip> >> /root/.ssh/known_hosts
```

## OCI guests are detect-and-pull, not rebuild

Proxmox squashes every image layer into one rootfs when it creates the guest.
There is no image left to swap, so there is no in-place update — the guest *is*
the unpacked image, plus whatever has happened to it since.

`oci-images` therefore reports drift and, with `--pull-oci`, fetches the newer
image. It stops there. Rebuilding means reproducing the guest's network,
mountpoints, resources and lifecycle, and getting any of that subtly wrong
loses data silently. That belongs behind a per-guest decision.

Guests opt in by carrying a tag that records their source image:

```bash
lighthouse proxmox --node pve1 --tag-guest 3136 --image docker.io/library/alpine:3.20
```

The tag is necessary because Proxmox stores no link between a guest and the
image it came from, and the template filename keeps only the last path element
and tag — `docker.io/library/alpine:3.20` becomes `alpine_3.20.tar`, losing the
registry and namespace irrecoverably.

Until a guest is rebuilt, `lxc-packages` is the only thing that will patch it.
The two providers are not alternatives.

## In-container package updates

`docker-packages` is opt-in and should stay that way for anything built from a
maintained image. It fights the model: the container's filesystem is ephemeral,
so every upgrade is discarded the next time the image is pulled and the
container recreated. For an image-managed container the correct fix is a newer
image, which `docker-images` already does.

It exists for the cases where that is not true — an image abandoned upstream,
one built locally and not rebuilt on a schedule, a long-lived container nobody
is going to recreate this quarter. Patching those in place is worse than
rebuilding and much better than leaving them.

```bash
lighthouse agent --providers docker-packages --include legacy-app --apply
```

## Scheduling

```bash
# One run, then exit. Good for a systemd timer or CI.
lighthouse agent --node pve1

# Nightly at 04:00, in the container's TZ.
lighthouse agent --node pve1 --schedule "0 0 4 * * *"

# Or an interval.
lighthouse agent --node pve1 --interval 6h
```

Scheduling is the agent's own rather than an external timer, so one process
holds the run lock. Two overlapping runs would have two package managers
contending for the same `dpkg` lock on the same guest.

## Filtering

`--include` and `--exclude` take names or globs, and match against both the
target ID and its name, so you need not know which form Lighthouse stores:

```bash
lighthouse agent --node pve1 --include 'ct:31*'
lighthouse agent --node pve1 --exclude plex --exclude 'cv-*'
```

## Deployment

Lighthouse runs in all three runtimes it manages:

| Runtime | See |
|---|---|
| Docker | `deploy/docker/compose.agent.yml` |
| LXC / bare host | `deploy/lxc/install.sh` |
| Proxmox OCI guest | `deploy/oci/README.md` |

The agent image is `dockerfiles/Dockerfile.agent`, which is Alpine-based rather
than scratch. It carries an ssh client, because reaching a Proxmox node needs
one and a scratch image has no binaries at all.
