package data

import (
	"context"
	"fmt"
	"time"

	"log"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const collectionName = "vehicles"

// FirestoreRepository handles data persistence to GCP Firestore.
type FirestoreRepository struct {
	client *firestore.Client
}

// NewFirestoreRepository creates a new instance.
func NewFirestoreRepository(client *firestore.Client) *FirestoreRepository {
	return &FirestoreRepository{
		client: client,
	}
}

// SaveVehicles uses Firestore Batch write to insert multiple telemetry points at once.
func (r *FirestoreRepository) SaveVehicles(ctx context.Context, vehicles []Vehicle) error {
	if len(vehicles) == 0 {
		return nil
	}

	// Firestore limits batches to 500 operations.
	// In a complete implementation, we'd chunk this if len(vehicles) > 500.
	// For demo sizing, it's typically fine.
	batch := r.client.Batch()
	col := r.client.Collection(collectionName)

	for _, v := range vehicles {
		// Use auto-generated document IDs for time-series append-only logging
		docRef := col.NewDoc()
		v.ID = docRef.ID
		if v.IngestedAt.IsZero() {
			v.IngestedAt = time.Now().UTC()
		}
		// Drive the Firestore TTL policy: expire 24h after ingestion, safely
		// beyond the 12h history read window. Firestore reclaims the storage
		// for free, keeping this append-only collection in the free tier.
		v.ExpireAt = v.IngestedAt.Add(24 * time.Hour)
		batch.Set(docRef, v)
	}

	_, err := batch.Commit(ctx)
	if err != nil {
		return fmt.Errorf("failed to commit vehicle batch: %w", err)
	}

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

// VehiclePathGroup is a grouped set of path points for a single vehicle,
// used by the /api/vehicles/paths endpoint to pre-seed frontend trails.
type VehiclePathGroup struct {
	VehicleID string      `json:"vehicleId"`
	Points    []PathPoint `json:"points"`
}

// PathPoint is a single position in a vehicle's historical trail.
type PathPoint struct {
	Latitude   float64   `json:"latitude"`
	Longitude  float64   `json:"longitude"`
	IngestedAt time.Time `json:"ingestedAt"`
}

// GetVehiclePaths retrieves historical positions for all vehicles of a given
// source within the time window, grouped by vehicle_id. Each group's points
// are sorted oldest-first so the frontend can render a fading trail directly.
//
// Uses the existing DESC composite index (same as GetRecentVehicles) and
// reverses each group's points in memory to produce oldest-first order.
func (r *FirestoreRepository) GetVehiclePaths(ctx context.Context, source string, since time.Time) ([]VehiclePathGroup, error) {
	q := r.client.Collection(collectionName).
		Where("source", "==", source).
		Where("ingested_at", ">=", since).
		OrderBy("ingested_at", firestore.Desc)

	iter := q.Documents(ctx)
	groups := make(map[string][]PathPoint)

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
			continue
		}

		if v.VehicleID == "" {
			continue
		}

		// Prepend to get oldest-first order from DESC results
		groups[v.VehicleID] = append([]PathPoint{{
			Latitude:   v.Latitude,
			Longitude:  v.Longitude,
			IngestedAt: v.IngestedAt,
		}}, groups[v.VehicleID]...)
	}

	result := make([]VehiclePathGroup, 0, len(groups))
	for vehicleID, points := range groups {
		result = append(result, VehiclePathGroup{
			VehicleID: vehicleID,
			Points:    points,
		})
	}

	log.Printf("GetVehiclePaths (%s since %s): %d vehicles, %d total points",
		source, since.Format("15:04:05"), len(result), func() int {
			total := 0
			for _, g := range result {
				total += len(g.Points)
			}
			return total
		}())

	return result, nil
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
