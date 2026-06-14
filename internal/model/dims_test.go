package model

import "testing"

// TestValidMemberName pins the member-name contract (SPEC §2): the character
// regex plus the calc-referenceability rule — every valid name must round-trip
// as an A# formula reference (SPEC §6), so hyphens must be tightly bound and a
// name may not end in a space.
func TestValidMemberName(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		valid bool
	}{
		// plain names
		{"simple", "Sales", true},
		{"digits ok", "4100", true},
		{"interior space", "North America", true},
		{"dots", "Acct.1.x", true},
		{"underscore", "Net_Sales", true},
		{"trailing dot", "X.", true},

		// hyphens: legal only when tightly bound by name-word chars
		{"interior hyphen", "Co-Op", true},
		{"hyphen before digit", "X-2", true},
		{"leading hyphen", "-X", false},
		{"trailing hyphen", "X-", false},
		{"hyphen before space", "X- Y", false},
		{"hyphen after space", "X -Y", false},

		// spaces and forbidden characters
		{"trailing space", "North America ", false},
		{"leading space", " Sales", false},
		{"empty", "", false},
		{"pipe forbidden", "A|B", false},
		{"hash forbidden", "A#B", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidMemberName(tt.in)
			if (err == nil) != tt.valid {
				t.Fatalf("ValidMemberName(%q) err=%v, want valid=%v", tt.in, err, tt.valid)
			}
		})
	}
}
