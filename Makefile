INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-unpin

# Coverage output lives under $(HOME), not /tmp: the duva e2e suites run the
# binary in a container writing to a bind mount, and on macOS Docker Desktop
# only shares certain host paths -- /tmp is not one of them.
COVER_DIR   := $(HOME)/.cache/docker-pin-cover

.PHONY: all build install clean docker-duva test test-unit test-e2e cover cover-html

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
# test-unit needs nothing. test-e2e needs Docker and network: it exercises
# real registries, real containers and real compose files, which is the whole
# point -- the unit tests fake those, so they cannot catch a tag being
# rewritten, a label failing to survive a parse, or the queue not rendering.

test: test-unit test-e2e

test-unit:
	go test ./...

test-e2e:
	./hack/e2e-pin.sh
	./hack/e2e-duva.sh
	./hack/e2e-duva-policy.sh

# --- coverage ---------------------------------------------------------
# Reports unit and e2e coverage separately, then merged. Merging matters:
# neither number alone is honest. Unit tests cannot reach internal/docker at
# all (it shells out to a daemon), and the e2e suites barely touch the pure
# logic the unit tests cover exhaustively.
#
# See go.dev/blog/integration-test-coverage for the mechanism: binaries are
# built with -cover and write to $GOCOVERDIR, which go tool covdata merges
# with the unit tests' own profiles.
cover:
	@rm -rf $(COVER_DIR) && mkdir -p $(COVER_DIR)/unit $(COVER_DIR)/e2e $(COVER_DIR)/merged
	@echo "== unit"
	@go test ./... -cover -args -test.gocoverdir=$(COVER_DIR)/unit >/dev/null
	@go tool covdata percent -i=$(COVER_DIR)/unit | sort
	@echo
	@echo "== e2e"
	@GOCOVERDIR=$(COVER_DIR)/e2e ./hack/e2e-pin.sh >/dev/null
	@GOCOVERDIR=$(COVER_DIR)/e2e ./hack/e2e-duva.sh >/dev/null
	@GOCOVERDIR=$(COVER_DIR)/e2e ./hack/e2e-duva-policy.sh >/dev/null
	@go tool covdata percent -i=$(COVER_DIR)/e2e | sort
	@echo
	@echo "== merged"
	@go tool covdata merge -i=$(COVER_DIR)/unit,$(COVER_DIR)/e2e -o=$(COVER_DIR)/merged
	@go tool covdata percent -i=$(COVER_DIR)/merged | sort
	@go tool covdata textfmt -i=$(COVER_DIR)/merged -o=$(COVER_DIR)/merged.txt
	@echo
	@echo "TOTAL: $$(go tool cover -func=$(COVER_DIR)/merged.txt | tail -1 | awk '{print $$NF}')"

cover-html: cover
	go tool cover -html=$(COVER_DIR)/merged.txt

clean:
	rm -f $(BINARIES) duva
	rm -rf $(COVER_DIR)
