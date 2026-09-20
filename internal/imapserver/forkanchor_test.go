package imapserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The package is a vendored fork of go-imap/v2's imapserver (see README.md).
// This test fails when go.mod's module pin and the README anchor diverge.
func TestForkAnchorMatchesModulePin(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	anchor := regexp.MustCompile("(?m)^Upstream anchor: `(v[^`]+)`").FindSubmatch(readme)
	if anchor == nil {
		t.Fatal(`README.md lost its "Upstream anchor: v..." line`)
	}

	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^\s*github\.com/emersion/go-imap/v2 (v\S+)`).FindSubmatch(gomod)
	if pin == nil {
		t.Fatal("go.mod no longer requires github.com/emersion/go-imap/v2 directly")
	}

	if string(anchor[1]) != string(pin[1]) {
		t.Fatalf("fork drift: go.mod pins go-imap/v2 %s but imapserver was vendored at %s — "+
			"run scripts/vendor-diff.sh imapserver, re-apply the extension hooks, and update the README anchor",
			pin[1], anchor[1])
	}
	if !strings.Contains(string(readme), "scripts/vendor-diff.sh") {
		t.Fatal("README.md must keep pointing at scripts/vendor-diff.sh")
	}
}
