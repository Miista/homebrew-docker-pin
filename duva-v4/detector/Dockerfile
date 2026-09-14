# The detector: what exists, and when.
#
# It lists tags and reports the ones published since it last looked. It knows
# nothing about versions -- not semver, not calver -- because deciding what a
# tag means belongs to the gate, and two things that both understood version
# ordering would be two things that had to agree.
#
# It holds no docker socket and writes nothing but one timestamp. /compose is
# read-only, which is the whole of what it can do to the host.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build
WORKDIR /src

# The context is the repository root, not duva-v4: this module replaces four
# sibling modules by relative path, so they all have to be there. Copying the
# go.mod files first keeps the dependency download in its own layer, which the
# source changing does not invalidate.
COPY go.mod go.sum ./
COPY compose/go.mod compose/go.sum ./compose/
COPY dockerapi/go.mod dockerapi/go.sum ./dockerapi/
COPY oci/go.mod ./oci/
COPY duva-v4/go.mod duva-v4/go.sum ./duva-v4/
RUN cd duva-v4 && go mod download

COPY . .
ARG VERSION=dev
# Set by buildx from the target platform. The build stage runs natively (see
# --platform above) and Go cross-compiles, which is both far faster than
# emulation and actually reliable: the Go toolchain under QEMU dies with
# "sync: inconsistent mutex state" on this repo.
ARG TARGETARCH
RUN cd duva-v4 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/detector ./detector
# An empty directory to become /data, so the final stage has something to
# copy -- see the note on ownership below.
RUN mkdir -p /out/data
# An empty directory to become /data, so the final stage has something to
# copy -- see the note on ownership below.
RUN mkdir -p /out/data

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
COPY --from=build /out/detector /detector

# /data exists in the image, owned by the user that runs. Docker chowns a
# fresh named volume to match what it copies out of the image -- but only if
# there is something there to copy. An empty VOLUME declaration gets a
# root-owned directory instead, and the detector then cannot record when it
# last checked: it does the whole run and fails on the final write, which
# means it re-reports the same tags forever.
COPY --from=build --chown=nonroot:nonroot /out/data /data

VOLUME ["/compose", "/data"]

# The detector serves no HTTP, so health is the binary answering about itself
# rather than a port something polls. Exec form with no shell, because this
# image has none.
#
# What it reports is whether the webhook can be reached: between daily runs
# that is the one thing both outside this process and fatal to its purpose,
# since a detector that cannot publish does all the work and drops the result.
# With no webhook configured it is healthy -- findings going only to the log is
# a real way to run this.
#
# Only meaningful in serve mode. A one-shot `run` exits, and Docker does not
# health-check a container that is not running.
HEALTHCHECK --interval=5m --timeout=10s --start-period=10s --retries=2 \
  CMD ["/detector", "health"]

USER nonroot:nonroot
ENTRYPOINT ["/detector"]
