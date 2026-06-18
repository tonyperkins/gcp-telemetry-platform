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
