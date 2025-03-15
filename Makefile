.PHONY: test build

test:
	go test ./... -v

build:
	go build -v -o build/golang-index ./...
