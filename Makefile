INSTALL_DIR := $(HOME)/.docker/cli-plugins
BINARIES     := docker-pin docker-upgrade

.PHONY: all build install clean

all: build

build:
	go build -o docker-pin  ./cmd/docker-pin
	go build -o docker-upgrade ./cmd/docker-upgrade

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 755 $(BINARIES) $(INSTALL_DIR)/

clean:
	rm -f $(BINARIES)
