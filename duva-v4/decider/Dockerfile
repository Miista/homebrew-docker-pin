# The decider: the gate.
#
# It reads the compose project, decides what a detector's notification means,
# and hands what policy allows to an actor. It talks to no registry and holds
# no docker socket -- the only thing it can do to this host is read a file and
# make an HTTP call to something that was configured for it.
#
# That is why this image carries nothing: no docker CLI, no git, no shell.
# Compare the actor's, which needs all three.

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
RUN cd duva-v4 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/decider ./decider

# distroless-static, not scratch.
#
# The decider talks to no registry, so certificates look unnecessary -- but it
# reaches the actor by container name, and resolving a name is the thing
# scratch breaks. A CGO-disabled static binary always uses Go's own resolver,
# which reads /etc/nsswitch.conf to decide resolution order; without it the
# detector was found to hang indefinitely on its first lookup rather than
# fail, and there is no reason to expect this to behave differently.
#
# So the base is the three things a networked static Go binary needs: the CA
# bundle, /etc/passwd for a named user, and the resolver configuration.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:6ec5aa99dc335666e79dc64e4a6c8b89c33a543a1967f20d360922a80dd21f02
COPY --from=build /out/decider /decider

# /compose is read-only: the decider reads what this host declares and never
# writes it. Writing is the actor's, and giving both the ability would make
# the split a convention rather than a constraint.
VOLUME ["/compose"]
EXPOSE 8080

# Health is "is it serving", asked over the loopback port it serves on. A
# subcommand rather than a port something polls, because this image carries
# neither a shell nor curl; exec form for the same reason.
#
# The compose file is not re-checked here. It is validated at startup and a
# decider that cannot read it refuses to start, so a process alive to answer
# this has already proved it. Re-reading in a second process would report on
# the file rather than on the server, which could be wedged while the file
# still parses perfectly.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/decider", "health"]

USER nonroot:nonroot
ENTRYPOINT ["/decider"]
