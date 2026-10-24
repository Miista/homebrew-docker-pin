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
#
# The binary is built by GoReleaser (see ../../.goreleaser.duva-v4.yaml), not
# here: this Dockerfile's context is GoReleaser's per-target output directory,
# containing only the binary this stage copies in.

# Alpine rather than distroless: the commit shells out to git, so there has to
# be a git to shell out to. Everything else the updater does goes through the
# daemon's API over the mounted socket, which is why there is no docker CLI.
FROM alpine:3.22@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
RUN apk add --no-cache git ca-certificates
# GoReleaser's dockers_v2 lays the per-platform binary out at
# linux/<arch>/<binary> in the build context, one platform per invocation of
# this Dockerfile -- so TARGETARCH (which buildx sets to match) picks the one
# this build is for.
ARG TARGETARCH
COPY linux/${TARGETARCH}/update /update

# No VOLUME. Where the project is mounted is the operator's choice --
# DUVA_UPDATE_COMPOSE_DIR -- because this stage has to be able to read it from
# the same path the host has, so a relative bind resolves to a string the
# daemon can also find. Declaring /compose here would make docker invent an
# empty anonymous volume there whenever the project is mounted anywhere else,
# which is exactly what it did: the updater read that empty directory and
# reported the repository as missing.
EXPOSE 8080
ENTRYPOINT ["/update"]
