.PHONY: verify fmt-check deps tidy vet test build fmt

## verify: every check CI runs (format, deps, vet, race tests, build)
verify: fmt-check deps vet test build

## fmt-check: fail on any unformatted Go file
fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "unformatted files:"; gofmt -l .; exit 1)

## deps: module integrity. This repo is a filtered subset of the private
## engine tree, whose go.mod legitimately carries commercial-only
## requirements for code stripped here — `go mod tidy -diff` would fail by
## design (and pruning them in exports would force a public-history
## rewrite on every change). go mod verify still enforces integrity
## against go.sum, and build fails on any actually-missing requirement.
deps:
	go mod verify

## tidy: reconcile go.mod/go.sum (manual; contributors may run freely,
## maintainers reconcile private-tree requirements on merge)
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
