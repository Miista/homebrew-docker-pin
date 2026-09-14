# The UI: the page.
#
# It holds no compose file, no docker socket and no registry credentials. It
# asks deciders what is queued and asks them to apply what someone clicked,
# and that is the whole of what it can do. This is the half that faces a
# browser, and it is deliberately the half with nothing worth reaching.
#
# Compare the decider's image, which at least reads the project, and the
# actor's, which holds the socket and carries git.

# Base images are pinned by digest, the same way this tool asks a compose file
# to pin its own: the tag says which stream to follow, the digest records what
# was actually built.
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
RUN cd duva-v4 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ui ./ui

# distroless-static, not scratch.
#
# The UI reaches deciders by container name, and resolving a name is the thing
# scratch breaks: a CGO-disabled static binary always uses Go's own resolver,
# which reads /etc/nsswitch.conf to decide resolution order, and without it a
# lookup was found to hang rather than fail. The CA bundle is here for the
# same reason it is in the decider's -- a decider reached over HTTPS rather
# than across a compose network is a configuration, not a rewrite.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:6ec5aa99dc335666e79dc64e4a6c8b89c33a543a1967f20d360922a80dd21f02
COPY --from=build /out/ui /ui

EXPOSE 8080

# Health is "is it serving", asked over the loopback port it serves on. A
# subcommand rather than a port something polls, because this image carries
# neither a shell nor curl; exec form for the same reason.
#
# Deliberately not a check on the deciders. An unreachable decider is this
# page's subject matter -- it renders as unreachable, which is the whole point
# of it -- and marking the UI unhealthy would take down the one thing that can
# tell you a decider is down.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/ui", "health"]

USER nonroot:nonroot
ENTRYPOINT ["/ui"]
