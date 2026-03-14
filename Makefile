BINARY  = nex
SRC     = ./cmd/nex/
TAGS    = fts5
BUILD   = build/ok
INSTALL = $(HOME)/.local/bin

.PHONY: build test lint fmt check install generate run clean

build:
	@mkdir -p build
	@pkill -x $(BINARY) 2>/dev/null && sleep 0.5 || true
	CGO_ENABLED=1 go build -tags "$(TAGS)" -o $(BINARY) $(SRC)
	@touch $(BUILD)

test:
	CGO_ENABLED=1 go test -tags $(TAGS) -v -count=1 -timeout 60s ./...

lint:
	golangci-lint run --build-tags $(TAGS) ./...

fmt:
	gofmt -w .

check: deps fmt vet test

deps:
	go mod tidy
	go mod verify

vet:
	go vet -tags $(TAGS) ./...

generate:
	go generate ./...

install: build
	@mkdir -p $(INSTALL)
	cp $(BINARY) $(INSTALL)/$(BINARY)

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY)
	rm -rf build/
