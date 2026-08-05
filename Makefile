# Build and test entry points for the Go implementation.
#
# Requirements: Go 1.25+. The integration and e2e suites additionally need a working
# docker or podman with the compose plugin.

BINARY  ?= hole
VERSION ?= development
LDFLAGS := -s -w -X github.com/lukashornych/hole/internal/version.Version=$(VERSION)

.PHONY: all build test itest e2e lint fmt golden clean

all: lint test build

## build: compile a static binary for the host platform
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/hole

## test: unit tests — no container runtime needed
test:
	go test ./...

## itest: integration tests — needs a real docker/podman daemon
##        -p 1: they share one daemon and globally-named resources (the image mirror, the
##        hole-sandbox-* namespace), and `uninstall` removes everything Hole owns by design, so
##        packages must not run in parallel
itest:
	go test -tags integration -count=1 -timeout 20m -p 1 ./...

## e2e: end-to-end sandbox tests with the built-in test agent — needs a real daemon and
##      pulls/builds sandbox images, so it is slow on a cold cache
##      -v: the suite runs for 10+ minutes and `go test` buffers a package's output to the end,
##      so without it the whole run is silent and indistinguishable from a hang
e2e:
	go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/

## lint: formatting and static analysis
lint:
	@unformatted=$$(gofmt -l cmd internal assets test); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go vet -tags integration ./...
	go vet -tags e2e ./...
	# Both shipped platforms: the terminal check in internal/engine is GOOS-specific, so a
	# host-only build would not notice a break on the other one until release.
	CGO_ENABLED=0 GOOS=linux go build ./...
	CGO_ENABLED=0 GOOS=darwin go build ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; \
	else echo "golangci-lint not installed, skipping"; fi

## fmt: rewrite files with gofmt
fmt:
	gofmt -w cmd internal assets test

## golden: regenerate golden files (compose files, gateway artifacts)
golden:
	go test ./internal/network/ ./internal/sandbox/ -update -count=1

clean:
	rm -f $(BINARY)
