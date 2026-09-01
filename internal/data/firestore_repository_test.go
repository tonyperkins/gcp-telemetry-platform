package data

import (
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestVehicleSignature(t *testing.T) {
	tests := []struct {
		name    string
		v       Vehicle
		wantOK  bool
		wantSig string
	}{
		{
			name:    "empty vehicle ID always writes",
			v:       Vehicle{Latitude: 30.2711, Longitude: -97.7437},
			wantOK:  false,
			wantSig: "",
		},
		{
			name:    "basic signature",
			v:       Vehicle{VehicleID: "2219", Latitude: 30.2711, Longitude: -97.7437, SpeedKmh: f(43.2)},
			wantOK:  true,
			wantSig: "30.271|-97.744|45",
		},
		{
			name:    "GPS jitter within ~111m rounds to same bucket",
			v:       Vehicle{VehicleID: "2219", Latitude: 30.27114, Longitude: -97.74371, SpeedKmh: f(43.2)},
			wantOK:  true,
			wantSig: "30.271|-97.744|45",
		},
		{
			name:    "speed rounds to nearest 5 km/h",
			v:       Vehicle{VehicleID: "2219", Latitude: 30.2711, Longitude: -97.7437, SpeedKmh: f(47.4)},
			wantOK:  true,
			wantSig: "30.271|-97.744|45",
		},
		{
			name:    "nil speed treated as 0",
			v:       Vehicle{VehicleID: "2219", Latitude: 30.2711, Longitude: -97.7437},
			wantOK:  true,
			wantSig: "30.271|-97.744|0",
		},
		{
			name:    "meaningful movement changes signature",
			v:       Vehicle{VehicleID: "2219", Latitude: 30.2750, Longitude: -97.7437, SpeedKmh: f(43.2)},
			wantOK:  true,
			wantSig: "30.275|-97.744|45",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := vehicleSignature(tt.v)
			if ok != tt.wantOK {
				t.Fatalf("vehicleSignature() ok = %v, want %v", ok, tt.wantOK)
			}
			if sig != tt.wantSig {
				t.Fatalf("vehicleSignature() = %q, want %q", sig, tt.wantSig)
			}
		})
	}
}

func TestShouldWriteDedup(t *testing.T) {
	r := NewFirestoreRepository(nil)
	now := time.Now()

	base := Vehicle{VehicleID: "2219", Latitude: 30.2711, Longitude: -97.7437, SpeedKmh: f(43.2)}

	// First sighting: always writes.
	if !r.shouldWrite(base, now) {
		t.Fatal("first sighting should write")
	}

	// Identical position (with GPS jitter): skipped.
	jittered := base
	jittered.Latitude += 0.00004
	jittered.Longitude -= 0.00004
	if r.shouldWrite(jittered, now.Add(time.Minute)) {
		t.Fatal("unchanged position (within jitter) should be skipped")
	}

	// Real movement: writes.
	moved := base
	moved.Latitude += 0.005 // ~550 m
	if !r.shouldWrite(moved, now.Add(2*time.Minute)) {
		t.Fatal("moved vehicle should write")
	}

	// Different vehicle, same position as another: writes (per-vehicle state).
	other := moved
	other.VehicleID = "9999"
	if !r.shouldWrite(other, now.Add(3*time.Minute)) {
		t.Fatal("different vehicle ID should write independently")
	}

	// Vehicle with no ID: always writes.
	noID := base
	noID.VehicleID = ""
	if !r.shouldWrite(noID, now.Add(4*time.Minute)) {
		t.Fatal("vehicle without ID should always write")
	}
}

func TestPruneDedupState(t *testing.T) {
	r := NewFirestoreRepository(nil)
	now := time.Now()

	old := Vehicle{VehicleID: "old-bus", Latitude: 30.27, Longitude: -97.74}
	fresh := Vehicle{VehicleID: "new-bus", Latitude: 30.28, Longitude: -97.75}

	r.shouldWrite(old, now.Add(-48*time.Hour))
	r.shouldWrite(fresh, now)

	r.pruneDedupState(now)

	if _, exists := r.lastWritten["old-bus"]; exists {
		t.Fatal("vehicle not seen in 24h should be pruned")
	}
	if _, exists := r.lastWritten["new-bus"]; !exists {
		t.Fatal("recent vehicle should be retained")
	}
}
