# labs — build, test and package.
#
# The same targets are what CI runs, so "it works locally" and "it works in the
# build" are one answer rather than two.

MODULE  := github.com/shaowenchen/labs
BIN     := bin
IMAGE   ?= docker.io/shaowenchen/labs
TAG     ?= dev
# The version and commit are stamped into the binary so a running service can
# say what it is, which is the first question anyone asks of one.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
  -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT)

GO ?= go
# -mod=mod so a target works from a clean checkout; CI sets its own GOFLAGS.
GOFLAGS ?= -mod=mod

.PHONY: all
all: fmt vet test build

.PHONY: build
build:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/labs ./cmd/labs

# A local run: the state file goes to a scratch path so it does not need /data,
# and the token is taken from the environment. It starts even with no reachable
# environment — /healthz answers and /readyz says what is not ready.
.PHONY: run
run: build
	LABS_STATE_FILE=./bin/labs-state.json $(BIN)/labs

.PHONY: test
test:
	$(GO) test ./...

# The race detector needs cgo, so this overrides the CGO_ENABLED=0 the builds
# use. It is worth having: the store and the rate limiter are the two places a
# request and a reaper can touch the same memory.
.PHONY: test-race
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: coverage
coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

# fmt-check fails rather than rewriting, so CI never edits a tree it is meant to
# be judging.
.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "these files are not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: check
check: fmt-check vet test

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out

.PHONY: docker-build
docker-build:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  -t $(IMAGE):$(TAG) .

.PHONY: docker-push
docker-push:
	docker push $(IMAGE):$(TAG)
