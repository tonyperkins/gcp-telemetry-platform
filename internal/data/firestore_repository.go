package data

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"log"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const collectionName = "vehicles"

// vehicleState is the quantized last-written signature for a vehicle, used to
// skip redundant writes (see SaveVehicles).
type vehicleState struct {
	signature string
	updatedAt time.Time
}

// FirestoreRepository handles data persistence to GCP Firestore.
type FirestoreRepository struct {
	client *firestore.Client

	// In-memory dedup state. Safe because polling is single-instance in
	// practice (1 req / 5 min never scales past one Cloud Run instance); on a
	// cold start the cache is empty and one full write cycle occurs.
	mu          sync.Mutex
	lastWritten map[string]vehicleState
}

// NewFirestoreRepository creates a new instance.
func NewFirestoreRepository(client *firestore.Client) *FirestoreRepository {
	return &FirestoreRepository{
		client:      client,
		lastWritten: make(map[string]vehicleState),
	}
}

// vehicleSignature quantizes a vehicle's position and speed into a comparable
// string. Coordinates are rounded to 3 decimals (~111 m) to absorb GPS jitter,
// speed to the nearest 5 km/h. Heading is intentionally excluded: it is noisy
// at low speeds, and any vehicle whose heading changes meaningfully is moving
// and therefore changes position too.
func vehicleSignature(v Vehicle) (string, bool) {
	if v.VehicleID == "" {
		return "", false
	}
	lat := math.Round(v.Latitude*1000) / 1000
	lng := math.Round(v.Longitude*1000) / 1000
	speed := 0.0
	if v.SpeedKmh != nil {
		speed = math.Round(*v.SpeedKmh/5) * 5
	}
	return fmt.Sprintf("%.3f|%.3f|%.0f", lat, lng, speed), true
}

// shouldWrite reports whether the vehicle's state changed enough to warrant a
// new document, and records the new state when it does. Caller must hold r.mu.
func (r *FirestoreRepository) shouldWrite(v Vehicle, now time.Time) bool {
	sig, ok := vehicleSignature(v)
	if !ok {
		return true // no vehicle ID: always write (read path skips these anyway)
	}
	if prev, exists := r.lastWritten[v.VehicleID]; exists && prev.signature == sig {
		prev.updatedAt = now
		r.lastWritten[v.VehicleID] = prev
		return false
	}
	r.lastWritten[v.VehicleID] = vehicleState{signature: sig, updatedAt: now}
	return true
}

// pruneDedupState drops vehicles not seen in 24h so retired buses/flights
// don't accumulate. Caller must hold r.mu.
func (r *FirestoreRepository) pruneDedupState(now time.Time) {
	cutoff := now.Add(-24 * time.Hour)
	for id, st := range r.lastWritten {
		if st.updatedAt.Before(cutoff) {
			delete(r.lastWritten, id)
		}
	}
}

// SaveVehicles uses Firestore Batch write to insert multiple telemetry points
// at once. Vehicles whose quantized position/speed is unchanged since the last
// write are skipped: the feed reports every vehicle (including hundreds of
// stationary buses) on every poll, and writing them all verbatim dominated the
// Firestore bill (~15K docs/hour). Moving vehicles always produce new points.
func (r *FirestoreRepository) SaveVehicles(ctx context.Context, vehicles []Vehicle) error {
	if len(vehicles) == 0 {
		return nil
	}

	now := time.Now().UTC()

	// Firestore limits batches to 500 operations. Dedup only shrinks the
	// batch, so a single pass is safe for demo-scale feeds.
	batch := r.client.Batch()
	col := r.client.Collection(collectionName)

	r.mu.Lock()
	r.pruneDedupState(now)
	written := 0
	for _, v := range vehicles {
		if !r.shouldWrite(v, now) {
			continue
		}
		// Use auto-generated document IDs for time-series append-only logging
		docRef := col.NewDoc()
		v.ID = docRef.ID
		if v.IngestedAt.IsZero() {
			v.IngestedAt = now
		}
		// Drive the Firestore TTL policy: expire 24h after ingestion, safely
		// beyond the 12h history read window. Firestore reclaims the storage
		// for free, keeping this append-only collection from growing unbounded.
		v.ExpireAt = v.IngestedAt.Add(24 * time.Hour)
		batch.Set(docRef, v)
		written++
	}
	r.mu.Unlock()

	if written == 0 {
		return nil
	}

	_, err := batch.Commit(ctx)
	if err != nil {
		return fmt.Errorf("failed to commit vehicle batch: %w", err)
	}

	log.Printf("SaveVehicles: wrote %d/%d vehicles (%d skipped as unchanged)", written, len(vehicles), len(vehicles)-written)
	return nil
}

// GetRecentVehicles retrieves vehicle positions by source within a time window.
// Backed by a Firestore composite index on (source, ingested_at).
func (r *FirestoreRepository) GetRecentVehicles(ctx context.Context, source string, since time.Time) ([]Vehicle, error) {
	q := r.client.Collection(collectionName).
		Where("source", "==", source).
		Where("ingested_at", ">=", since).
		OrderBy("ingested_at", firestore.Desc)

	iter := q.Documents(ctx)

	// Create a map to deduplicate by VehicleID, keeping only the newest point for each vehicle.
	// Since we OrderBy "ingested_at" Desc, the first point we see for each ID is the newest.
	newestVehicles := make(map[string]Vehicle)
	var vehicleOrder []string // To preserve the Desc sorting order

	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to iterate documents: %w", err)
		}

		var v Vehicle
		if err := doc.DataTo(&v); err != nil {
			return nil, fmt.Errorf("failed to parse document: %w", err)
		}
		v.ID = doc.Ref.ID

		// Robust ID extraction: use struct field but fallback to map if missing
		uniqueID := v.VehicleID
		if uniqueID == "" {
			if dataMap := doc.Data(); dataMap != nil {
				if mID, ok := dataMap["vehicle_id"].(string); ok {
					uniqueID = mID
				}
			}
		}

		// If we still have no ID, we MUST skip it for deduplication to work.
		// Using document ID as fallback (previous behavior) caused count inflation (2k+ buses).
		if uniqueID == "" {
			continue
		}

		// If this is the first time we've seen this vehicleId (newest due to OrderBy), keep it.
		if _, exists := newestVehicles[uniqueID]; !exists {
			v.VehicleID = uniqueID // Ensure it's populated for the frontend
			newestVehicles[uniqueID] = v
			vehicleOrder = append(vehicleOrder, uniqueID)
		}
	}

	results := make([]Vehicle, 0, len(vehicleOrder))
	for _, id := range vehicleOrder {
		results = append(results, newestVehicles[id])
	}

	log.Printf("GetRecentVehicles (%s): deduplicated documents into %d unique vehicles", source, len(results))

	return results, nil
}

// GetUniqueRoutes scans recent telemetry to find all active route IDs.
func (r *FirestoreRepository) GetUniqueRoutes(ctx context.Context, source string, since time.Time) ([]string, error) {
	// We only select route_id for efficiency
	q := r.client.Collection(collectionName).
		Where("source", "==", source).
		Where("ingested_at", ">=", since).
		Select("route_id")

	iter := q.Documents(ctx)
	routeMap := make(map[string]bool)

	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}

		if dataMap := doc.Data(); dataMap != nil {
			if rid, ok := dataMap["route_id"].(string); ok && rid != "" && rid != "Unmapped" {
				routeMap[rid] = true
			}
		}
	}

	routes := make([]string, 0, len(routeMap))
	for rid := range routeMap {
		routes = append(routes, rid)
	}
	return routes, nil
}

// GetVehicleHistory retrieves historical positions for a specific vehicle.
// Backed by a Firestore composite index on (vehicle_id, ingested_at).
func (r *FirestoreRepository) GetVehicleHistory(ctx context.Context, vehicleID string, since time.Time) ([]Vehicle, error) {
	q := r.client.Collection(collectionName).
		Where("vehicle_id", "==", vehicleID).
		Where("ingested_at", ">=", since).
		OrderBy("ingested_at", firestore.Desc)

	iter := q.Documents(ctx)
	var results []Vehicle

	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to iterate documents: %w", err)
		}

		var v Vehicle
		if err := doc.DataTo(&v); err != nil {
			return nil, fmt.Errorf("failed to parse document: %w", err)
		}
		v.ID = doc.Ref.ID
		results = append(results, v)
	}

	return results, nil
}
