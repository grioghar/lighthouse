FROM --platform=$BUILDPLATFORM alpine:3.19.0 as alpine

RUN apk add --no-cache \
    ca-certificates \
    tzdata

FROM scratch
# Canonical lighthouse identity label, plus the legacy watchtower label so
# existing tooling that detects the updater container keeps working.
LABEL "lighthouse"="true"
LABEL "com.centurylinklabs.watchtower"="true"

COPY --from=alpine \
    /etc/ssl/certs/ca-certificates.crt \
    /etc/ssl/certs/ca-certificates.crt
COPY --from=alpine \
    /usr/share/zoneinfo \
    /usr/share/zoneinfo

EXPOSE 8080

COPY lighthouse /

HEALTHCHECK CMD [ "/lighthouse", "--health-check"]

ENTRYPOINT ["/lighthouse"]
