BINARY  = nex
SRC     = ./cmd/nex/
TAGS    = fts5

.PHONY: build run test clean

build:
	@pkill -x $(BINARY) 2>/dev/null && sleep 0.5 || true
	CGO_ENABLED=1 go build -tags "$(TAGS)" -o $(BINARY) $(SRC)

run: build
	./$(BINARY)

test:
	CGO_ENABLED=1 go test -tags $(TAGS) -v -count=1 -timeout 60s ./...

clean:
	rm -f $(BINARY)
