GO ?= go

.PHONY: build test vet fmt e2e
build:
	mkdir -p bin
	$(GO) build -o bin/tsqld ./cmd/tsqld
	$(GO) build -o bin/tsql ./cmd/tsql
test:
	$(GO) test ./... -count=1
vet:
	$(GO) vet ./...
fmt:
	gofmt -l -w .
e2e:
	./test/e2e.sh
