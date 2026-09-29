# Build and install hostrunner. Binaries are built without cgo: the hostrun
# client is copied into containers with arbitrary images, so it must be
# statically linked (`hostrunner up` refuses a dynamic one).

PREFIX ?= $(HOME)/.local

.PHONY: build install test e2e

build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/hostrunner bin/hostrun $(PREFIX)/bin/

test:
	go test -race ./...

# Needs the devcontainer CLI and docker and/or podman; pulls a small image.
e2e:
	go test -tags e2e -count=1 -v ./e2e/
