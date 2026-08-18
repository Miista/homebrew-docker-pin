# duva is a static Go binary that only reads: it parses compose files
# and its config from mounted volumes and queries container registries over
# HTTPS. It never touches the Docker socket, never shells out to the
# docker CLI, and never mutates anything it's given — so the final image
# needs nothing beyond the binary and CA certs for TLS.

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/duva ./cmd/duva
# /data must exist in the image owned by nonroot: a named volume mounted
# there inherits this ownership on first use, so `- duva-state:/data` works
# without any user:/chown ceremony. (An absent dir would make Docker create
# the volume root-owned, and nonroot couldn't write the state file.)
RUN mkdir /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/duva /duva
ENTRYPOINT ["/duva"]
CMD ["serve"]
