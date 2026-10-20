# The UI: the page.
#
# It holds no compose file, no docker socket and no registry credentials. It
# asks queues what is queued and asks them to apply what someone clicked,
# and that is the whole of what it can do. This is the half that faces a
# browser, and it is deliberately the half with nothing worth reaching.
#
# Compare the queue's image, which at least reads the project, and the
# updater's, which holds the socket and carries git.
#
# The binary is built by GoReleaser (see ../../.goreleaser.duva-v4.yaml), not
# here: this Dockerfile's context is GoReleaser's per-target output directory,
# containing only the binary this stage copies in.

# distroless-static, not scratch.
#
# The UI reaches queues by container name, and resolving a name is the thing
# scratch breaks: a CGO-disabled static binary always uses Go's own resolver,
# which reads /etc/nsswitch.conf to decide resolution order, and without it a
# lookup was found to hang rather than fail. The CA bundle is here for the
# same reason it is in the queue's -- a queue reached over HTTPS rather
# than across a compose network is a configuration, not a rewrite.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:6ec5aa99dc335666e79dc64e4a6c8b89c33a543a1967f20d360922a80dd21f02
# GoReleaser's dockers_v2 lays the per-platform binary out at
# linux/<arch>/<binary> in the build context, one platform per invocation of
# this Dockerfile -- so TARGETARCH (which buildx sets to match) picks the one
# this build is for.
ARG TARGETARCH
COPY linux/${TARGETARCH}/ui /ui

EXPOSE 8080

# Health is "is it serving", asked over the loopback port it serves on. A
# subcommand rather than a port something polls, because this image carries
# neither a shell nor curl; exec form for the same reason.
#
# Deliberately not a check on the queues. An unreachable queue is this
# page's subject matter -- it renders as unreachable, which is the whole point
# of it -- and marking the UI unhealthy would take down the one thing that can
# tell you a queue is down.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/ui", "health"]

USER nonroot:nonroot
ENTRYPOINT ["/ui"]
