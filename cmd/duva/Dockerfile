# duva watches pinned compose services and applies the updates each service's
# policy allows: it pulls the image, rewrites the pin, recreates the container
# and commits the change. That needs the docker CLI, compose and git at
# runtime, so the final image cannot be distroless-static as it was while duva
# only reported.
#
# docker:cli carries all three. Assembling them by hand would only be a slower
# way of getting the same binaries.

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

FROM docker:cli@sha256:000bb62ff495f986c9f5578eb67cc2cb98b91138eda81d7762d5371eb8a497fe
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
