package fts

import (
	"testing"

	"github.com/blevesearch/bleve/v2/registry"
)

// TestCJKAnalyzerAvailable fails fast when the mailez_cjk analyzer is not
// registered (init in cjk.go).
func TestCJKAnalyzerAvailable(t *testing.T) {
	cache := registry.NewCache()
	an, err := cache.AnalyzerNamed(CJKAnalyzerName)
	if err != nil || an == nil {
		t.Fatalf("cjk analyzer unavailable: %v", err)
	}
}
