# duva is a static Go binary that only reads: it parses compose files
# and pin.yaml from a mounted volume and queries container registries over
# HTTPS. It never touches the Docker socket, never shells out to the
# docker CLI, and never mutates anything it's given — so the final image
# needs nothing beyond the binary and CA certs for TLS.

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/duva ./cmd/duva

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/duva /duva
ENTRYPOINT ["/duva"]
CMD ["serve"]
