.PHONY: verify fmt-check tidy vet test build fmt

## verify: every check CI runs (format, tidy, vet, race tests, build)
verify: fmt-check tidy vet test build

## fmt-check: fail on any unformatted Go file
fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "unformatted files:"; gofmt -l .; exit 1)

## tidy: fail if go.mod/go.sum drift
tidy:
	go mod tidy -diff

## vet: static analysis
vet:
	go vet ./...

## test: race-enabled unit tests
test:
	go test ./... -count=1 -race

## build: compile everything
build:
	go build ./...

## fmt: normalize formatting
fmt:
	gofmt -w .
