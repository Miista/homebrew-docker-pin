# The queue: the gate.
#
# It reads the compose project, decides what a watcher's notification means,
# and hands what policy allows to an updater. It talks to no registry and holds
# no docker socket -- the only thing it can do to this host is read a file and
# make an HTTP call to something that was configured for it.
#
# That is why this image carries nothing: no docker CLI, no git, no shell.
# Compare the updater's, which needs all three.
#
# The binary is built by GoReleaser (see ../../.goreleaser.duva-v4.yaml), not
# here: this Dockerfile's context is GoReleaser's per-target output directory,
# containing only the binary this stage copies in.

# An empty directory to become /data. Docker seeds a fresh named volume from
# the image, ownership included, so this is what makes the volume writable.
# A no-op FROM/RUN pair rather than a file GoReleaser has to be told to
# include, since an empty directory is not something a build context can
# carry.
FROM alpine:3.22@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1 AS data
RUN mkdir -p /data

# distroless-static, not scratch.
#
# The queue talks to no registry, so certificates look unnecessary -- but it
# reaches the updater by container name, and resolving a name is the thing
# scratch breaks. A CGO-disabled static binary always uses Go's own resolver,
# which reads /etc/nsswitch.conf to decide resolution order; without it the
# watcher was found to hang indefinitely on its first lookup rather than
# fail, and there is no reason to expect this to behave differently.
#
# So the base is the three things a networked static Go binary needs: the CA
# bundle, /etc/passwd for a named user, and the resolver configuration.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:6ec5aa99dc335666e79dc64e4a6c8b89c33a543a1967f20d360922a80dd21f02
# GoReleaser's dockers_v2 lays the per-platform binary out at
# linux/<arch>/<binary> in the build context, one platform per invocation of
# this Dockerfile -- so TARGETARCH (which buildx sets to match) picks the one
# this build is for.
ARG TARGETARCH
COPY linux/${TARGETARCH}/queue /queue

# /compose is read-only: the queue reads what this host declares and never
# writes it. Writing is the updater's, and giving both the ability would make
# the split a convention rather than a constraint.
VOLUME ["/compose"]
EXPOSE 8080

# Health is "is it serving", asked over the loopback port it serves on. A
# subcommand rather than a port something polls, because this image carries
# neither a shell nor curl; exec form for the same reason.
#
# The compose file is not re-checked here. It is validated at startup and a
# queue that cannot read it refuses to start, so a process alive to answer
# this has already proved it. Re-reading in a second process would report on
# the file rather than on the server, which could be wedged while the file
# still parses perfectly.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/queue", "health"]

# /data exists in the image, owned by the user this actually runs as.
#
# 1000, not nonroot: a `user:` in compose overrides the image's, and this runs
# as the host user that owns /compose. Docker seeds a fresh named volume with
# this directory's ownership, so getting the uid wrong here is a volume the
# queue cannot write -- measured, not assumed: chowned to nonroot's 65532 and
# run as 1000, every save fails on a container that otherwise looks healthy.
COPY --from=data --chown=1000:1000 /data /data

USER nonroot:nonroot
ENTRYPOINT ["/queue"]
