TAGS := -tags sqlite_fts5

build:
	go build $(TAGS) -o spore ./cmd/spore

test:
	go test $(TAGS) ./...

vet:
	go vet $(TAGS) ./...

fmt:
	gofmt -w .

fmtcheck:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: build test vet fmt fmtcheck
