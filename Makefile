.PHONY: verify fmt-check tidy vet test build fmt \
        verify-ee vet-ee test-ee build-ee check-ce-purity export-ce

## verify: every check CI runs (format, tidy, vet, race tests, build)
verify: fmt-check tidy vet test build

## verify-ee: the same battery for the enterprise edition (-tags mailez_ee)
verify-ee: fmt-check tidy vet-ee test-ee build-ee

## fmt-check: fail on any unformatted Go file
fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "unformatted files:"; gofmt -l .; exit 1)

## tidy: fail if go.mod/go.sum drift
tidy:
	go mod tidy -diff

## vet: static analysis (community edition)
vet:
	go vet ./...

## vet-ee: static analysis (enterprise edition)
vet-ee:
	go vet -tags mailez_ee ./...

## test: race-enabled unit tests (community edition)
test:
	go test ./... -count=1 -race

## test-ee: race-enabled unit tests (enterprise edition)
test-ee:
	go test -tags mailez_ee ./... -count=1 -race

## build: compile everything (community edition)
build:
	go build ./...

## build-ee: compile everything (enterprise edition)
build-ee:
	go build -tags mailez_ee ./...

## check-ce-purity: the community (no-tag) dependency graph must never
## reach internal/ee — the compiler-level half of the edition split — and
## every EE-tagged file must match the export strip contract (internal/ee/
## or *_ee.go / *_ee_test.go), or it would leak into the public tree.
check-ce-purity:
	@deps=$$(go list -deps ./... 2>/dev/null | grep -c 'mailezine/internal/ee'); \
	if [ "$$deps" != "0" ]; then \
		echo "CE build reaches internal/ee ($$deps packages) — forbidden"; \
		go list -deps ./... | grep 'mailezine/internal/ee'; \
		exit 1; \
	fi; \
	bad=$$(grep -rlE '^//go:build mailez_ee' --include='*.go' --exclude-dir=.git . | grep -vE '/internal/ee/' | grep -vE '_ee\.go$$|_ee_test\.go$$'); \
	if [ -n "$$bad" ]; then \
		echo "EE-tagged files outside the export strip contract (rename with an _ee suffix):"; \
		echo "$$bad"; \
		exit 1; \
	fi
	@echo "ce-purity: ok"

## export-ce: produce the public community source tree (stdout path list for
## CI). Requires git-filter-repo. The public mirror must never contain
## internal/ee, *_ee.go, or *_ee_test.go files.
export-ce:
	scripts/export-ce.sh

## fmt: normalize formatting
fmt:
	gofmt -w .
