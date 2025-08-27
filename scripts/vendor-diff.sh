#!/usr/bin/env bash
# Diff a vendored fork against the upstream module source pinned by go.mod.
#
# Usage:
#   scripts/vendor-diff.sh [imapserver|gosieve]
#
# Resolves the module version through `go list -m` (single source of truth),
# downloads it into the module cache, then prints a recursive diff of the
# vendored tree vs upstream. Markdown files (fork-local README/LICENSE notes)
# are excluded. Exit status reflects go/diff availability only; differences
# are expected output, not failures.
set -euo pipefail

cd "$(dirname "$0")/.."

case "${1:-}" in
imapserver)
	module=github.com/emersion/go-imap/v2
	upstream="$(go env GOMODCACHE)/$module@$(go list -m -f '{{.Version}}' "$module")/imapserver"
	local_dir=internal/imapserver
	;;
gosieve)
	module=github.com/foxcpp/go-sieve
	upstream="$(go env GOMODCACHE)/$module@$(go list -m -f '{{.Version}}' "$module")"
	local_dir=internal/gosieve
	;;
*)
	echo "usage: $0 [imapserver|gosieve]" >&2
	exit 2
	;;
esac

go mod download "$module"
echo "# local fork: $local_dir  |  upstream: $upstream" >&2
# Differences are expected; do not fail on them.
diff -ruN --exclude='*.md' --exclude='LICENSE' "$local_dir" "$upstream" || true
