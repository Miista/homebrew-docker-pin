# duva v5: one binary, one image.
#
# Released by GoReleaser from duva-v5/vX.Y tags (.goreleaser.duva-v5.yaml);
# this is for working on it locally.

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build clean test

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o duva ./cmd/duva

clean:
	rm -f duva

test:
	go test -shuffle=on ./...
