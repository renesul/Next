BINARY  = next
SRC     = .
TAGS    = fts5
INSTALL = $(HOME)/.next

.PHONY: build test lint fmt check install generate run clean docker-build docker-run docker-stop

build:
	@pkill -x $(BINARY) 2>/dev/null && sleep 0.5 || true
	CGO_ENABLED=1 go build -tags "$(TAGS)" -o $(BINARY) $(SRC)

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

docker-build:
	docker build -t next .

docker-run:
	docker compose up -d

docker-stop:
	docker compose down
