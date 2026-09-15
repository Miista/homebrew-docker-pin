# The updater: the thing that does the work.
#
# Pull the image, write the digest into the compose file, recreate the
# container, commit, push. It is the reference implementation of the contract
# in internal/update -- another updater answering the same two calls would be a
# valid replacement, and the queue could not tell.
#
# Everything it holds, it holds because applying needs it: the repository
# read-write, because the pin is rewritten there; the docker socket, to pull
# and to replace a container. Compare the queue's image, which holds neither
# and is distroless as a result. This one carries git, for the commit.

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
# Native build stage, Go cross-compiles: the toolchain under QEMU dies with
# "sync: inconsistent mutex state" partway through compiling.
ARG TARGETARCH
RUN cd duva-v4 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/update ./update

# Alpine rather than distroless: the commit shells out to git, so there has to
# be a git to shell out to. Everything else the updater does goes through the
# daemon's API over the mounted socket, which is why there is no docker CLI.
FROM alpine:3.22@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/update /update

# /compose is read-write here, unlike the queue's: rewriting the pin is the
# whole point. Giving both the ability would make the split a convention
# rather than a constraint.
VOLUME ["/compose"]
EXPOSE 8080
ENTRYPOINT ["/update"]
