# duva v4: detector, decider, actor, ui.
#
# Its own build, because it is its own thing. The binaries are released and
# versioned together -- they speak one contract and a version skew between
# them is a bug -- but separately from `docker pin`, which they are not part of.

BINARIES    := detector decider actor ui
VERSION     ?= dev
LDFLAGS     := -s -w -X main.version=$(VERSION)
REGISTRY    ?= registry.example.com

.PHONY: all build clean test images push

all: build

build:
	@for b in $(BINARIES); do \
		echo "building $$b"; \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $$b ./$$b || exit 1; \
	done

clean:
	rm -f $(BINARIES)

test:
	go test -shuffle=on ./...

# Each binary has its own Dockerfile, because each needs a different thing:
# the detector and decider hold no docker socket and shell out to nothing; the
# actor holds the socket and carries git, for the commit. One shared image
# would give all three the union of what any of them needs, which is the
# opposite of why they are separate processes.
images:
	@for b in $(BINARIES); do \
		echo "building image for $$b"; \
		docker build -q -f $$b/Dockerfile \
			--build-arg VERSION=$(VERSION) \
			-t $(REGISTRY)/duva-$$b:$(VERSION) .. || exit 1; \
	done

push: images
	@for b in $(BINARIES); do \
		docker push $(REGISTRY)/duva-$$b:$(VERSION) || exit 1; \
	done
