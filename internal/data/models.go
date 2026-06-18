package data

import "time"

// Vehicle represents the unified schema for all telemetry sources in Firestore.
// We use omitempty for nullable fields so they are omitted from Firestore documents.
type Vehicle struct {
	ID         string    `firestore:"id,omitempty" json:"id"` // Document ID
	Source     string    `firestore:"source" json:"source"`   // 'metro' or 'flight'
	VehicleID  string    `firestore:"vehicle_id" json:"vehicleId"`
	Label      string    `firestore:"label,omitempty" json:"label,omitempty"`
	RouteID    string    `firestore:"route_id,omitempty" json:"routeId,omitempty"`
	TripID     string    `firestore:"trip_id,omitempty" json:"tripId,omitempty"`
	Latitude   float64   `firestore:"latitude" json:"latitude"`
	Longitude  float64   `firestore:"longitude" json:"longitude"`
	AltitudeM  *float64  `firestore:"altitude_m,omitempty" json:"altitudeM,omitempty"` // null for ground vehicles
	SpeedKmh   *float64  `firestore:"speed_kmh,omitempty" json:"speedKmh,omitempty"`
	Heading    *float64  `firestore:"heading,omitempty" json:"heading,omitempty"`    // degrees 0-360
	OnGround   *bool     `firestore:"on_ground,omitempty" json:"onGround,omitempty"` // flight only; 1=on ground
	RawJSON    string    `firestore:"raw_json,omitempty" json:"rawJson,omitempty"`   // full source payload for replay
	IngestedAt time.Time `firestore:"ingested_at" json:"ingestedAt"`
}
