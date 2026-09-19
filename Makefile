TAGS := -tags sqlite_fts5

build:
	go build $(TAGS) -o spore ./cmd/spore

# PREFIX is where `make install` puts the binary. ~/.local/bin is on the
# default PATH of a normal login shell and needs no privileges.
PREFIX ?= $(HOME)/.local

install: build
	install -d $(PREFIX)/bin
	install -m 0755 spore $(PREFIX)/bin/spore

test:
	go test $(TAGS) ./...

# test-weaviate needs a running vector store: `spore recall setup` first.
# It is not part of `make test` on purpose -- the default suite must not
# depend on a container.
test-weaviate:
	go test -tags "sqlite_fts5 weaviate" ./internal/recall/... -v

# test-phoenix needs a running collector: `spore trace setup` first.
# It is not part of `make test` on purpose -- the default suite must not
# depend on a container.
test-phoenix:
	go test -tags "sqlite_fts5 phoenix" ./internal/trace/... -v

vet:
	go vet $(TAGS) ./...

fmt:
	gofmt -w .

fmtcheck:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# The lint gate. CI runs this exact target, so a green run here is a green
# run there. The version is pinned on purpose: `latest` lets a golangci-lint
# release turn master red with no commit of ours.
GOLANGCI_VERSION := v2.13.2

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

lint:
	golangci-lint run ./...

# The dependency gate, pinned and run through a target for the same reason
# as the linter: CI and a developer must run the same command.
GOVULNCHECK_VERSION := v1.8.0

vulncheck-install:
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

vulncheck:
	govulncheck -tags sqlite_fts5 ./...

tidycheck:
	go mod tidy && git diff --exit-code go.mod go.sum

.PHONY: build install test test-weaviate test-phoenix vet fmt fmtcheck lint lint-install vulncheck vulncheck-install tidycheck
