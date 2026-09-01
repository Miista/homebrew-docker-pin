INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-unpin

# Coverage output lives under $(HOME), not /tmp: the duva integration suites run the
# binary in a container writing to a bind mount, and on macOS Docker Desktop
# only shares certain host paths -- /tmp is not one of them.
COVER_DIR   := $(HOME)/.cache/docker-pin-cover

.PHONY: all build install clean docker-duva test test-unit test-integration cover cover-html conditions

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
	go test -shuffle=on ./...

# -p 1 keeps packages sequential: the suites share a registry port and a
# testbed, and duva does not run several copies of itself in production
# either.
#
# -shuffle=on because these tests share one testbed directory, one registry
# and one daemon, so a test can pass on what a previous one left behind. That
# is not hypothetical: a fixed order hid a test which only ever passed because
# another had created a directory for it first. Go prints the seed, and
# -shuffle=<seed> replays a failure exactly.
test-integration:
	go test -tags integration -count=1 -p 1 -shuffle=on ./test/...

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
	@GOCOVERDIR=$(COVER_DIR)/integration go test -tags integration -count=1 -p 1 -shuffle=on ./test/... >/dev/null
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

# --- condition coverage --------------------------------------------------
# `go test -cover` counts statements: whether a line ran, not whether both
# ways through it were taken. That is blind to a whole class of bug -- duva
# recorded a notification it had failed to send, and both lines ran in every
# test; what was never exercised was the path where the first fails and the
# second happens anyway.
#
# gobco instruments each condition and reports the ones that were only ever
# true, or only ever false. Not part of `make test`: it is a question you ask
# while writing tests, and most of what it reports is error plumbing that is
# not worth a fault injector.
#
# Install with: go install github.com/rillig/gobco@latest
#
# The mkdir is a workaround, not a courtesy: gobco walks the module and fails
# outright if a directory it saw is gone, and the suites delete testbed*/ when
# they finish.
# Every package with tests, less the ones whose conditions are scaffolding:
# the integration suite and its receiver, the sandbox, the fixture generator
# and the man-page tool exist to test or build other things, and their branches
# are not duva's behaviour.
CONDITION_PKGS ?= $(shell go list ./... \
	| sed 's|^github.com/Miista/homebrew-docker-pin/||' \
	| grep -vE '^(github.com|test/|tools/|internal/fixture)')
GOBCO ?= $(shell command -v gobco 2>/dev/null || echo $(shell go env GOPATH)/bin/gobco)

conditions:
	@test -x "$(GOBCO)" || { \
		echo "gobco not found; install with: go install github.com/rillig/gobco@latest"; \
		exit 1; \
	}
	@mkdir -p testbed-scratch
	@for p in $(CONDITION_PKGS); do \
		echo "== $$p"; \
		(cd $$p && "$(GOBCO)" 2>&1 | grep -vE "^(ok|---|PASS|FAIL)[[:space:]]" ) || true; \
	done
	@rmdir testbed-scratch 2>/dev/null || true
