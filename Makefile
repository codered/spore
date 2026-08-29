TAGS := -tags sqlite_fts5

build:
	go build $(TAGS) -o spore ./cmd/spore

test:
	go test $(TAGS) ./...

vet:
	go vet $(TAGS) ./...

.PHONY: build test vet
