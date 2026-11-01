package postgres

import "testing"

func TestNormalizeJobUUID(t *testing.T) {
	const lower = "f2d5a4c8-4735-4242-8a70-ef42fb6db936"
	for _, input := range []string{lower, "F2D5A4C8-4735-4242-8A70-EF42FB6DB936"} {
		got, ok := normalizeJobUUID(input)
		if !ok || got != lower {
			t.Errorf("normalizeJobUUID(%q) = (%q, %t), want (%q, true)", input, got, ok, lower)
		}
	}
	for _, input := range []string{"", "not-a-uuid", "f2d5a4c8-4735-4242-0a70-ef42fb6db936", "f2d5a4c8-4735-5242-8a70-ef42fb6db936"} {
		if got, ok := normalizeJobUUID(input); ok {
			t.Errorf("normalizeJobUUID(%q) = (%q, true), want invalid", input, got)
		}
	}
}
