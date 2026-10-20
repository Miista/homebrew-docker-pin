# The watcher: what exists, and when.
#
# It lists tags and reports the ones published since it last looked. It knows
# nothing about versions -- not semver, not calver -- because deciding what a
# tag means belongs to the gate, and two things that both understood version
# ordering would be two things that had to agree.
#
# It holds no docker socket and writes nothing but one timestamp. /compose is
# read-only, which is the whole of what it can do to the host.
#
# The binary is built by GoReleaser (see ../../.goreleaser.duva-v4.yaml), not
# here: this Dockerfile's context is GoReleaser's per-target output directory,
# containing only the binary this stage copies in.

# An empty directory to become /data, so the final stage has something to
# copy -- see the note on ownership below. A no-op FROM/RUN pair rather than a
# file GoReleaser has to be told to include, since an empty directory is not
# something a build context can carry.
FROM alpine:3.22@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1 AS data
RUN mkdir -p /data

# distroless-static, not scratch.
#
# scratch was tried and does not work: the same binary hangs indefinitely on
# the first registry call, where on this base it finishes in fourteen seconds.
# A CGO-disabled static binary always uses Go's own resolver, which reads
# /etc/nsswitch.conf to decide resolution order -- and without that file it
# hangs rather than failing, which is the worst of both outcomes. Certificates
# were not the problem; those were carried either way.
#
# So this base is not padding. It is the three things a networked static Go
# binary needs and nothing else: the CA bundle, /etc/passwd for a named user,
# and the resolver configuration. Five megabytes for a program that otherwise
# stops working.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:6ec5aa99dc335666e79dc64e4a6c8b89c33a543a1967f20d360922a80dd21f02
# GoReleaser's dockers_v2 lays the per-platform binary out at
# linux/<arch>/<binary> in the build context, one platform per invocation of
# this Dockerfile -- so TARGETARCH (which buildx sets to match) picks the one
# this build is for.
ARG TARGETARCH
COPY linux/${TARGETARCH}/watch /watch

# /data exists in the image, owned by the user that runs. Docker chowns a
# fresh named volume to match what it copies out of the image -- but only if
# there is something there to copy. An empty VOLUME declaration gets a
# root-owned directory instead, and the watcher then cannot record when it
# last checked: it does the whole run and fails on the final write, which
# means it re-reports the same tags forever.
COPY --from=data --chown=nonroot:nonroot /data /data

VOLUME ["/compose", "/data"]

# The watcher serves no HTTP, so health is the binary answering about itself
# rather than a port something polls. Exec form with no shell, because this
# image has none.
#
# What it reports is whether the webhook can be reached: between daily runs
# that is the one thing both outside this process and fatal to its purpose,
# since a watcher that cannot publish does all the work and drops the result.
# With no webhook configured it is healthy -- findings going only to the log is
# a real way to run this.
#
# Only meaningful in serve mode. A one-shot `run` exits, and Docker does not
# health-check a container that is not running.
HEALTHCHECK --interval=5m --timeout=10s --start-period=10s --retries=2 \
  CMD ["/watch", "health"]

USER nonroot:nonroot
ENTRYPOINT ["/watch"]
