package bookings

import (
	"math"
	"strings"
	"testing"
)

func TestValidateRules(t *testing.T) {
	long := strings.Repeat("x", MaxReferenceLength+1)
	cases := []struct {
		reference string
		teu       int64
		want      string
	}{
		{"REF-1", 1, ""},
		{"  REF-1  ", MaxTeuPerBooking, ""},
		{strings.Repeat("x", MaxReferenceLength), 1, ""},
		{"", 1, "Reference is required."},
		{"   ", 1, "Reference is required."},
		{long, 1, "Reference must be at most 40 characters."},
		{"REF-1", 0, "TEU must be at least 1."},
		{"REF-1", -3, "TEU must be at least 1."},
		{"REF-1", MaxTeuPerBooking + 1, "A booking holds at most 20 TEU."},
		{"REF-1", 4294967297, "A booking holds at most 20 TEU."},
		{"REF-1", math.MaxInt64, "A booking holds at most 20 TEU."},
	}
	for _, c := range cases {
		if got := Validate(c.reference, c.teu); got != c.want {
			t.Errorf("Validate(%q, %d) = %q, want %q", c.reference, c.teu, got, c.want)
		}
	}
}

func TestFeeIsAFeePerTeuPlusAFlatFee(t *testing.T) {
	for _, teu := range []int{1, 2, MaxTeuPerBooking} {
		got, err := FeeCents(teu)
		if want := int64(teu)*FeeCentsPerTeu + FlatFeeCents; err != nil || got != want {
			t.Errorf("FeeCents(%d) = %d, %v; want %d", teu, got, err, want)
		}
	}
}

func TestFeeThrowsInsteadOfWrapping(t *testing.T) {
	for _, teu := range []int{math.MaxInt32, -1} {
		if _, err := FeeCents(teu); teu < 0 && err == nil {
			t.Errorf("FeeCents(%d) must fail", teu)
		}
	}
}
