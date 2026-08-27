# duva watches pinned compose services and applies the updates each service's
# policy allows: it pulls the image, rewrites the pin, recreates the container
# and commits the change. That needs the docker CLI, compose and git at
# runtime, so the final image cannot be distroless-static as it was while duva
# only reported.
#
# docker:cli carries all three. Assembling them by hand would only be a slower
# way of getting the same binaries.

FROM golang:1.26-alpine AS build
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

FROM docker:cli
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
