# Lighthouse agent as an OCI image for Proxmox's oci-registry-pull.
#
# Proxmox creates an LXC guest from an OCI image by squashing every layer into
# one rootfs. The result is a normal LXC container, so what matters here is
# that the image works as a whole filesystem with an init, not that it is a
# minimal container image:
#
#   - No scratch base. The guest needs a shell, an ssh client, and enough of a
#     userland to be debuggable from the Proxmox console.
#   - The entrypoint runs as PID 1. It must not exit, which is why the agent
#     is started with a schedule rather than as a one-shot.
#   - Layers are squashed at creation, so this image cannot be updated in
#     place. Updating the agent means pulling a new image and recreating the
#     guest -- which is exactly what the oci-images provider reports on.
#
# Build and push:
#   docker build -f deploy/oci/Dockerfile -t docker.io/<you>/lighthouse-agent:0.1.0 .
#   docker push docker.io/<you>/lighthouse-agent:0.1.0
#
# Then on the node, see deploy/oci/README.md.
FROM golang:1.25-alpine AS builder

ARG VERSION=oci

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a \
    -ldflags "-extldflags '-static' -X github.com/grioghar/lighthouse/internal/meta.Version=${VERSION}" \
    -o /out/lighthouse .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates openssh-client tzdata tini

COPY --from=builder /out/lighthouse /usr/local/bin/lighthouse
COPY deploy/oci/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

LABEL "lighthouse"="true"
LABEL "lighthouse.agent"="true"

# tini reaps zombies. As PID 1 in an LXC guest the agent forks ssh once per
# guest per run, and without a reaper those accumulate until the pid limit.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
