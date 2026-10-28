// Package bookings is the HTTP side of the saga and the handler that moves an booking forward as events arrive.
package bookings

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Booking rules.
const (
	MaxTeuPerBooking   = 20
	MaxReferenceLength = 40
	FeeCentsPerTeu     = 90_000
	FlatFeeCents       = 7_500
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// Validate returns an error message, or "" when the request is acceptable. The teu count is compared as int64 on purpose:
// narrowing 4294967297 to int32 would wrap to 1 and pass.
func Validate(reference string, teu int64) string {
	trimmed := strings.TrimSpace(reference)
	switch {
	case trimmed == "":
		return "Reference is required."
	case len([]rune(trimmed)) > MaxReferenceLength:
		return fmt.Sprintf("Reference must be at most %d characters.", MaxReferenceLength)
	case teu < 1:
		return "TEU must be at least 1."
	case teu > MaxTeuPerBooking:
		return fmt.Sprintf("A booking holds at most %d TEU.", MaxTeuPerBooking)
	}
	return ""
}

// FeeCents is a fee per teu plus a flat fee, in cents. Overflow is an error, not a wrap.
func FeeCents(teu int) (int64, error) {
	if teu < 0 || int64(teu) > (math.MaxInt64-FlatFeeCents)/FeeCentsPerTeu {
		return 0, ErrOverflow
	}
	return int64(teu)*FeeCentsPerTeu + FlatFeeCents, nil
}
