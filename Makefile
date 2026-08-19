INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-unpin

.PHONY: all build install clean docker-duva

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

clean:
	rm -f $(BINARIES) duva
