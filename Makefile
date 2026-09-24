.PHONY: build fmt-check lint test test-integration release-check

build:
	go build -o hawkeye .

fmt-check:
	@test -z "$$(gofmt -l *.go)" || { gofmt -l *.go; exit 1; }

lint: fmt-check
	golangci-lint run

test:
	go test -race ./...

test-integration:
	HAWKEYE_DOCKER_TEST=1 go test -race ./...

release-check:
	goreleaser release --snapshot --clean --skip=publish
