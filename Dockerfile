# Root Dockerfile: builds Lighthouse from the build context with no extra flags,
# so `docker build https://github.com/grioghar/lighthouse.git` and a compose
# `build:` stanza that points at the repo work out of the box. (Building from a
# git URL previously failed with "open Dockerfile: no such file or directory"
# because the Dockerfiles only lived under dockerfiles/.)
#
# For the release image that copies a pre-built binary, see dockerfiles/Dockerfile.
#
# Optionally override the embedded version: --build-arg VERSION=v1.2.3
FROM golang:1.25-alpine AS builder

ARG VERSION=docker

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# Download modules first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a \
    -ldflags "-extldflags '-static' -X github.com/grioghar/lighthouse/internal/meta.Version=${VERSION}" \
    -o /out/lighthouse .

FROM scratch

# Canonical lighthouse identity label, plus the legacy watchtower label so
# existing tooling that detects the updater container keeps working.
LABEL "lighthouse"="true"
LABEL "com.centurylinklabs.watchtower"="true"

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /out/lighthouse /lighthouse

EXPOSE 8080

HEALTHCHECK CMD [ "/lighthouse", "--health-check"]

ENTRYPOINT ["/lighthouse"]
