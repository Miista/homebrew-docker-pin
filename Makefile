INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-unpin

.PHONY: all build install clean

all: build

build:
	go build -trimpath -o docker-pin   ./cmd/docker-pin
	go build -trimpath -o docker-unpin ./cmd/docker-unpin

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 755 $(BINARIES) $(INSTALL_DIR)/

clean:
	rm -f $(BINARIES)
