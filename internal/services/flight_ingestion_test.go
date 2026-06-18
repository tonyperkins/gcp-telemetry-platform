package services

import (
	"testing"
	"time"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
)

func TestNormalizeFlights(t *testing.T) {
	now := time.Date(2026, 6, 18, 15, 0, 0, 0, time.UTC)
	existing := time.Date(2026, 6, 18, 14, 59, 0, 0, time.UTC)

	in := []data.Vehicle{
		{VehicleID: "abc123", Source: "wrong"},                 // source must be forced to "flight"
		{VehicleID: "def456", Source: "flight", IngestedAt: existing}, // existing timestamp preserved
	}

	out := normalizeFlights(in, now)

	if len(out) != 2 {
		t.Fatalf("expected 2 vehicles, got %d", len(out))
	}
	for i, v := range out {
		if v.Source != "flight" {
			t.Errorf("vehicle %d: source = %q, want %q", i, v.Source, "flight")
		}
	}
	if !out[0].IngestedAt.Equal(now) {
		t.Errorf("vehicle 0: zero IngestedAt should be filled with now=%v, got %v", now, out[0].IngestedAt)
	}
	if !out[1].IngestedAt.Equal(existing) {
		t.Errorf("vehicle 1: existing IngestedAt should be preserved (%v), got %v", existing, out[1].IngestedAt)
	}
}
