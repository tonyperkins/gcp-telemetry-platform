package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
	"google.golang.org/protobuf/proto"
)

// MetroIngestionService handles fetching and parsing GTFS-Realtime from Metro.
type MetroIngestionService struct {
	client *http.Client
	repo   *data.FirestoreRepository
}

// NewMetroIngestionService creates a new instance.
func NewMetroIngestionService(client *http.Client, repo *data.FirestoreRepository) *MetroIngestionService {
	return &MetroIngestionService{
		client: client,
		repo:   repo,
	}
}

// FetchAndSave fetches metro vehicles and saves them to Firestore.
func (s *MetroIngestionService) FetchAndSave(ctx context.Context, feedUrl string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", feedUrl, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("metro feed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metro returned status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err)
	}

	if len(body) > 10 && body[0] == '<' {
		return fmt.Errorf("feed returned HTML instead of protobuf")
	}

	vehicles, err := s.parseGtfsRt(body)
	if err != nil {
		return fmt.Errorf("failed to parse gtfs-rt: %w", err)
	}

	if len(vehicles) == 0 {
		return nil
	}

	if err := s.repo.SaveVehicles(ctx, vehicles); err != nil {
		return fmt.Errorf("failed to save metro vehicles: %w", err)
	}

	log.Printf("Ingested %d metro vehicles", len(vehicles))
	return nil
}

func (s *MetroIngestionService) parseGtfsRt(body []byte) ([]data.Vehicle, error) {
	feed := &gtfs.FeedMessage{}
	err := proto.Unmarshal(body, feed)
	if err != nil {
		return nil, fmt.Errorf("protobuf unmarshal failed: %w", err)
	}

	var vehicles []data.Vehicle
	now := time.Now().UTC()
	skippedNoVehicle := 0
	skippedNoPosition := 0
	skippedInvalidCoords := 0

	for _, entity := range feed.GetEntity() {
		vp := entity.GetVehicle()
		if vp == nil {
			skippedNoVehicle++
			continue
		}

		pos := vp.GetPosition()
		if pos == nil {
			skippedNoPosition++
			continue
		}

		lat := pos.GetLatitude()
		lon := pos.GetLongitude()

		if lat == 0 && lon == 0 {
			skippedInvalidCoords++
			continue
		}

		vId := vp.GetVehicle().GetId()
		if vId == "" {
			vId = entity.GetId()
		}

		label := vp.GetTrip().GetRouteId()
		if label == "" {
			label = vp.GetVehicle().GetLabel()
		}
		if label == "" {
			label = vId
		}

		// speed in GTFS-RT is m/s
		var speedKmh *float64
		if pos.Speed != nil {
			kmh := float64(pos.GetSpeed()) * 3.6
			speedKmh = &kmh
		}

		var heading *float64
		if pos.Bearing != nil {
			b := float64(pos.GetBearing())
			heading = &b
		}

		// Determine the best RouteID
		routeId := vp.GetTrip().GetRouteId()
		if routeId == "" {
			// Some feeds put the route in the vehicle label or use it as a fallback
			// If it looks like a short number or common Austin route code, it might be the route
			if label != "" && len(label) <= 4 {
				routeId = label
			} else {
				routeId = "Unmapped"
			}
		}

		vehicle := data.Vehicle{
			Source:     "metro",
			VehicleID:  vId,
			Label:      label,
			RouteID:    routeId,
			TripID:     vp.GetTrip().GetTripId(),
			Latitude:   float64(lat),
			Longitude:  float64(lon),
			SpeedKmh:   speedKmh,
			Heading:    heading,
			IngestedAt: now,
		}

		vehicles = append(vehicles, vehicle)
	}

	log.Printf("Parsed %d vehicles from GTFS-RT. Skipped: %d no veh, %d no pos, %d invalid coords",
		len(vehicles), skippedNoVehicle, skippedNoPosition, skippedInvalidCoords)

	return vehicles, nil
}
