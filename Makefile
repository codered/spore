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

.PHONY: build install test test-weaviate test-phoenix vet fmt fmtcheck
