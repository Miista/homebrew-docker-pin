INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-unpin

# Coverage output lives under $(HOME), not /tmp: the duva integration suites run the
# binary in a container writing to a bind mount, and on macOS Docker Desktop
# only shares certain host paths -- /tmp is not one of them.
COVER_DIR   := $(HOME)/.cache/docker-pin-cover

.PHONY: all build install clean docker-duva test test-unit test-integration cover cover-html

all: build

build:
	go build -trimpath -o docker-pin   ./cmd/docker-pin
	go build -trimpath -o docker-unpin ./cmd/docker-unpin
	go build -trimpath -o duva         ./cmd/duva

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 755 $(BINARIES) $(INSTALL_DIR)/

# docker-duva builds the duva container image locally (linux/amd64
# and linux/arm64 for x86/pi hosts respectively; CI publishes the multi-arch
# image to ghcr.io on release).
docker-duva:
	docker build -f cmd/duva/Dockerfile -t duva:dev .

# --- tests ------------------------------------------------------------
# test-unit needs nothing but Go. test-integration needs Docker, and runs
# everything locally: its own registry, its own images, its own notification
# receiver. Nothing upstream is contacted, so it works offline -- and a
# scenario cannot fail because someone published a new tag.
#
# They run sequentially: the suites share a registry port, and duva does not
# run several copies of itself in production either.

test: test-unit test-integration

test-unit:
	go test ./...

# -p 1 keeps packages sequential: the suites share a registry port and a
# testbed, and duva does not run several copies of itself in production
# either.
test-integration:
	go test -tags integration -count=1 -p 1 ./test/...

# --- coverage ---------------------------------------------------------
# Reports unit and integration coverage separately, then merged. Merging
# matters: neither number alone is honest. Unit tests cannot reach
# internal/docker at all (it shells out to a daemon), and the integration
# suites barely touch the pure logic the unit tests cover exhaustively.
#
# See go.dev/blog/integration-test-coverage for the mechanism: binaries are
# built with -cover and write to $GOCOVERDIR, which go tool covdata merges
# with the unit tests' own profiles.
cover:
	@rm -rf $(COVER_DIR) && mkdir -p $(COVER_DIR)/unit $(COVER_DIR)/integration $(COVER_DIR)/merged
	@echo "== unit"
	@# -coverpkg=./... attributes coverage to the package a statement lives in,
	@# not the package whose test ran it. Without it, cmd/duva's tests
	@# exercising internal/watch count for nothing and the report lies.
	@go test ./... -coverpkg=./... -cover -args -test.gocoverdir=$(COVER_DIR)/unit >/dev/null
	@go tool covdata percent -i=$(COVER_DIR)/unit | sort
	@echo
	@echo "== integration"
	@# The suites run duva as a container built with -cover, which writes to
	@# the mounted GOCOVERDIR. That reaches code no unit test can: the serve
	@# loop, the HTTP handlers, and everything that only runs against a real
	@# daemon.
	@GOCOVERDIR=$(COVER_DIR)/integration go test -tags integration -count=1 -p 1 ./test/... >/dev/null
	@go tool covdata percent -i=$(COVER_DIR)/integration | sort
	@echo
	@echo "== merged"
	@go tool covdata merge -i=$(COVER_DIR)/unit,$(COVER_DIR)/integration -o=$(COVER_DIR)/merged
	@go tool covdata percent -i=$(COVER_DIR)/merged | sort
	@go tool covdata textfmt -i=$(COVER_DIR)/merged -o=$(COVER_DIR)/merged.txt
	@echo
	@echo "TOTAL: $$(go tool cover -func=$(COVER_DIR)/merged.txt | tail -1 | awk '{print $$NF}')"

cover-html: cover
	go tool cover -html=$(COVER_DIR)/merged.txt

clean:
	rm -f $(BINARIES) duva
	rm -rf $(COVER_DIR)
