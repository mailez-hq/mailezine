// RFC 5256 SORT integration: the server orders messages by the requested
// keys with an optional search filter.
package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestIMAPSort(t *testing.T) {
	c, _ := startTestServer(t)
	if !c.Caps().Has(imap.CapSort) {
		t.Fatal("SORT capability not advertised")
	}
	appendMessage(t, c, "INBOX", "From: z@x.test\r\nSubject: alpha\r\n\r\n1\r\n", nil)
	appendMessage(t, c, "INBOX", "From: a@x.test\r\nSubject: beta\r\n\r\n2\r\n", nil)
	appendMessage(t, c, "INBOX", "From: m@x.test\r\nSubject: gamma\r\n\r\n3\r\n", nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// Sort by FROM ascending: a(2) m(3) z(1) → seqs [2 3 1].
	nums, err := c.Sort(&imapclient.SortOptions{
		SortCriteria:   []imapclient.SortCriterion{{Key: imapclient.SortKeyFrom}},
		SearchCriteria: &imap.SearchCriteria{},
	}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(nums) != 3 || nums[0] != 2 || nums[1] != 3 || nums[2] != 1 {
		t.Fatalf("sort by from = %v, want [2 3 1]", nums)
	}

	// REVERSE FROM: [1 3 2].
	nums, err = c.Sort(&imapclient.SortOptions{
		SortCriteria:   []imapclient.SortCriterion{{Key: imapclient.SortKeyFrom, Reverse: true}},
		SearchCriteria: &imap.SearchCriteria{},
	}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(nums) != 3 || nums[0] != 1 || nums[1] != 3 || nums[2] != 2 {
		t.Fatalf("reverse sort by from = %v, want [1 3 2]", nums)
	}

	// SORT with a search filter: only messages from a@x.test (seq 2).
	nums, err = c.Sort(&imapclient.SortOptions{
		SortCriteria:   []imapclient.SortCriterion{{Key: imapclient.SortKeyFrom}},
		SearchCriteria: &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: "a@x.test"}}},
	}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(nums) != 1 || nums[0] != 2 {
		t.Fatalf("filtered sort = %v, want [2]", nums)
	}
}
