package junk

import "strings"

// scoreAuthResults scores the signals in an RFC 8601 Authentication-Results
// header body (everything after "Authentication-Results:"). Each method is
// merged to its worst result so multiple dkim= segments for one message do
// not stack penalties.
func scoreAuthResults(ar string) []hit {
	if ar == "" {
		return nil
	}
	ar = strings.TrimPrefix(strings.TrimPrefix(ar, "Authentication-Results:"), " ")
	var (
		spf      = "" // worst spf result seen: fail > softfail > neutral > none/pass
		dkimFail = false
		dmarc    = ""
		policy   = ""
	)
	for _, seg := range strings.Split(ar, ";") {
		seg = strings.TrimSpace(seg)
		lower := strings.ToLower(seg)
		switch {
		case strings.HasPrefix(lower, "spf="):
			r := resValue(lower)
			if worseSPF(r, spf) {
				spf = r
			}
		case strings.HasPrefix(lower, "dkim="):
			if resValue(lower) == "fail" {
				dkimFail = true
			}
		case strings.HasPrefix(lower, "dmarc="):
			r := resValue(lower)
			if r == "fail" {
				dmarc = "fail"
				policy = dmarcPolicy(seg)
			} else if r != "pass" && dmarc == "" {
				dmarc = r
			}
		}
	}
	var hits []hit
	switch spf {
	case "fail":
		hits = append(hits, hit{"spf=fail", scoreSPFFail})
	case "softfail":
		hits = append(hits, hit{"spf=softfail", scoreSPFSoftFail})
	case "neutral":
		hits = append(hits, hit{"spf=neutral", scoreSPFNeutral})
	}
	if dkimFail {
		hits = append(hits, hit{"dkim=fail", scoreDKIMFail})
	}
	if dmarc == "fail" {
		if policy == "reject" {
			hits = append(hits, hit{"dmarc=fail (p=reject)", scoreDMARCReject})
		} else {
			hits = append(hits, hit{"dmarc=fail", scoreDMARCQuar})
		}
	}
	return hits
}

// resValue extracts the result token right after '=' ("spf=fail ..." -> "fail").
func resValue(seg string) string {
	eq := strings.Index(seg, "=")
	if eq < 0 {
		return ""
	}
	rest := seg[eq+1:]
	if sp := strings.IndexAny(rest, " \t("); sp >= 0 {
		rest = rest[:sp]
	}
	return strings.TrimSpace(rest)
}

// dmarcPolicy digs the "(p=reject)" style policy comment out of a dmarc segment.
func dmarcPolicy(seg string) string {
	op := strings.Index(seg, "(")
	cp := strings.LastIndex(seg, ")")
	if op < 0 || cp <= op {
		return ""
	}
	comment := strings.ToLower(seg[op : cp+1])
	for _, p := range []string{"reject", "quarantine", "none"} {
		if strings.Contains(comment, "p="+p) {
			return p
		}
	}
	return ""
}

// worseSPF orders spf results by how strongly they implicate the sender.
func worseSPF(a, b string) bool {
	rank := map[string]int{"fail": 4, "softfail": 3, "neutral": 2, "none": 1, "pass": 0, "temperror": 0, "permerror": 0}
	return rank[a] > rank[b]
}
