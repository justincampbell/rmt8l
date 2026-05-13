VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null | sed 's/-[0-9]*-g/-g/' || echo dev)
LDFLAGS := -X main.version=$(VERSION)
GOLANGCI_LINT_VERSION := v2.11.4
GOBIN := $(or $(shell go env GOBIN),$(shell go env GOPATH)/bin)

.PHONY: all build install test lint lint-gomod clean help

help:  ## Print this help
	@awk 'BEGIN {FS = ":.*##"; print "Usage: make <target>\n\nTargets:"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: lint test  ## Lint then test

build:  ## Build the rmt8l binary in the current directory
	go build -ldflags "$(LDFLAGS)" -o rmt8l .
	cp rmt8l "rmt8l@$(VERSION)"

install:  ## Build and install rmt8l (and a versioned copy) into $GOBIN
	go build -ldflags "$(LDFLAGS)" -o "$(GOBIN)/rmt8l" .
	cp "$(GOBIN)/rmt8l" "$(GOBIN)/rmt8l@$(VERSION)"
	@echo "Installed $(GOBIN)/rmt8l and $(GOBIN)/rmt8l@$(VERSION)"

test:  ## Run all tests
	go test ./...

lint: lint-gomod  ## Run golangci-lint
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

lint-gomod:  ## Verify go.mod / go.sum are tidy
	@go mod tidy && \
	if [ -n "$$(git diff --name-only go.mod go.sum)" ]; then \
		echo "error: go.mod is not tidy. Run 'go mod tidy' and commit the result." >&2; \
		git checkout go.mod go.sum; \
		exit 1; \
	fi

clean:  ## Remove built binaries
	rm -f rmt8l rmt8l@*
