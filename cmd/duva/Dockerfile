# duva watches pinned compose services and applies the updates each service's
# policy allows: it pulls the image, rewrites the pin, recreates the container
# and commits the change.
#
# All of the docker work goes through the daemon's API over the mounted
# socket, so the image needs no docker CLI -- only git, for the commit.

# Base images are pinned by digest, the same way this tool asks a compose file
# to pin its own: the tag says which stream to follow, the digest records what
# was actually built. A floating base means this image cannot be rebuilt as it
# was -- which is the problem docker pin exists to solve, so its own build
# should not have it.
#
# The digests are refreshed deliberately, by resolving the tags again.

FROM golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# COVER=1 builds a coverage-instrumented binary, so the integration suites --
# which exercise duva as a real container against a real registry -- can
# report coverage that unit tests structurally cannot reach
# (go.dev/blog/integration-test-coverage). Run it with GOCOVERDIR pointed at a
# mounted directory. Unset, this is an ordinary build.
ARG COVER=0
RUN if [ "$COVER" = "1" ]; then \
      CGO_ENABLED=0 go build -cover -coverpkg=./... -trimpath -o /out/duva ./cmd/duva; \
    else \
      CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/duva ./cmd/duva; \
    fi

# Alpine with git, which is all duva needs at runtime. It reaches the docker
# daemon over its socket rather than through the CLI, so there is nothing to
# install for that -- the same way Watchtower and WUD work, and for the same
# reason: the daemon exposes everything the CLI would have run.
FROM alpine:3@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache git ca-certificates
# /data must exist in the image: a named volume mounted there inherits this
# directory's ownership on first use, so `- duva-state:/data` works without
# any chown ceremony.
RUN mkdir -p /data
COPY --from=build /out/duva /duva
# The approval queue. Reachable only if the operator publishes it; duva always
# listens, since a container's port is invisible until someone maps it.
EXPOSE 8080
ENTRYPOINT ["/duva"]
CMD ["serve"]
