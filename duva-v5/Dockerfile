# duva v5: one image for every component.
#
# v4 built one image per component so each held only what it needed. In v5
# they run as one process (docs/duva-v5-one-binary.md), so the image is the
# updater's -- the one that needs the most: git for the commit, and CA
# certificates for registries. Everything else goes through the daemon's API
# over the mounted socket, so there is no docker CLI.
#
# The binary is built by GoReleaser, not here: this Dockerfile's context is
# GoReleaser's per-target output directory, with the binary at
# linux/<arch>/duva.
FROM alpine:3.22@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
RUN apk add --no-cache git ca-certificates \
 && mkdir -p /data \
 && chown 1000:1000 /data

# GoReleaser's dockers_v2 runs this once per platform, and buildx sets
# TARGETARCH to match, which picks that platform's binary.
ARG TARGETARCH
COPY linux/${TARGETARCH}/duva /duva

# /data is owned by uid 1000 in the image. Docker seeds a fresh named volume
# from the image, ownership included, and duva runs as the user that owns the
# repository -- set with `user:` in compose, which is 1000 on both hosts. A
# volume owned by anyone else is a volume duva cannot keep its state in.
#
# No VOLUME for the repository: it is mounted at the path it has on the host
# (DUVA_REPO), which is the operator's choice, and declaring one here would
# make docker invent an empty anonymous volume wherever it is not.
VOLUME ["/data"]
# 8080 is the queue API, for a UI on another host. 8081 is the page, when
# DUVA_UI is set -- apart, because the page is published and the API is not.
EXPOSE 8080 8081
# Exec form, no shell: duva asks its own listeners over loopback.
HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
  CMD ["/duva", "health"]
ENTRYPOINT ["/duva"]
