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

vet:
	go vet $(TAGS) ./...

fmt:
	gofmt -w .

fmtcheck:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: build install test vet fmt fmtcheck
