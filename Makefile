INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-unpin

.PHONY: all build install clean docker-tagwatch

all: build

build:
	go build -trimpath -o docker-pin   ./cmd/docker-pin
	go build -trimpath -o docker-unpin ./cmd/docker-unpin
	go build -trimpath -o tagwatch     ./cmd/tagwatch

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 755 $(BINARIES) $(INSTALL_DIR)/

# docker-tagwatch builds the tagwatch container image locally (linux/amd64
# and linux/arm64 for optiplex/pi respectively; CI publishes the multi-arch
# image to ghcr.io on release).
docker-tagwatch:
	docker build -f cmd/tagwatch/Dockerfile -t tagwatch:dev .

clean:
	rm -f $(BINARIES) tagwatch
