.PHONY: build run test test-coverage lint clean docker docker-down fmt vet

BINARY=xylolabs-kb
GO=/opt/homebrew/bin/go

build:
	$(GO) build -o bin/$(BINARY) ./cmd/$(BINARY)/

run: build
	./bin/$(BINARY)

test:
	$(GO) test -race ./...

test-coverage:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

lint:
	$(GO) vet ./...
	staticcheck ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf bin/ coverage.out coverage.html

docker:
	docker compose up --build

docker-down:
	docker compose down
