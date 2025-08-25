package acl

import "testing"

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"lrswipkxtecda", "lrswipkxtecda", true},
		{"al", "la", true},
		{"llrr", "lr", true}, // duplicates collapse
		{"rls", "lrs", true}, // canonical order restored
		{"", "", true},       // empty is a valid (zero) right set
		{"z", "", false},     // unknown right
		{"lr ", "", false},   // garbage
	} {
		got, err := Normalize(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Fatalf("Normalize(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestApplyModification(t *testing.T) {
	for _, tc := range []struct {
		cur, mod, want string
		ok             bool
	}{
		{"lr", "lrswip", "lrswip", true},  // bare string replaces
		{"lr", "+a", "lra", true},         // add normalizes order
		{"lrswipa", "-a", "lrswip", true}, // remove
		{"lr", "-x", "lr", true},          // removing absent right is a no-op
		{"", "", "", true},                // empty modification removes entry
		{"lr", "+z", "", false},           // unknown right
	} {
		got, err := ApplyModification(tc.cur, tc.mod)
		if (err == nil) != tc.ok || got != tc.want {
			t.Fatalf("ApplyModification(%q,%q) = %q, %v; want %q ok=%v", tc.cur, tc.mod, got, err, tc.want, tc.ok)
		}
	}
}

func TestValidateIdentifier(t *testing.T) {
	valid := []string{"bob@example.com", "anyone", "team", "-prefix"}
	for _, id := range valid {
		if err := ValidateIdentifier(id); err != nil {
			t.Fatalf("ValidateIdentifier(%q): %v", id, err)
		}
	}
	for _, id := range []string{"", "with space", "bad\"quote", "tab\there"} {
		if err := ValidateIdentifier(id); err == nil {
			t.Fatalf("ValidateIdentifier(%q) accepted", id)
		}
	}
}

func TestHas(t *testing.T) {
	if !Has("lr", 'l') || Has("lr", 'a') {
		t.Fatal("Has mismatch")
	}
}
