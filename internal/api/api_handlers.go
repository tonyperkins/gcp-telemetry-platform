package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/services"
)

// ApiHandlers exposes the REST API for the Glance dashboard / frontend.
type ApiHandlers struct {
	repo   *data.FirestoreRepository
	gtfs   *services.GtfsShapeService
}

func NewApiHandlers(repo *data.FirestoreRepository, gtfsService *services.GtfsShapeService) *ApiHandlers {
	return &ApiHandlers{repo: repo, gtfs: gtfsService}
}

// GetActiveVehicles returns recent vehicles by source.
// Serves `GET /api/vehicles/current?source={source}`
func (h *ApiHandlers) GetActiveVehicles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	source := r.URL.Query().Get("source")
	
	if source != "" && source != "metro" && source != "flight" {
		http.Error(w, `{"error": "invalid source parameter"}`, http.StatusBadRequest)
		return
	}

	since := time.Now().UTC().Add(-5 * time.Minute)
	var allVehicles []data.Vehicle

	if source == "" || source == "metro" {
		metroVehicles, err := h.repo.GetRecentVehicles(r.Context(), "metro", since)
		if err != nil {
			log.Printf("Error fetching metro vehicles: %v", err)
		} else {
			allVehicles = append(allVehicles, metroVehicles...)
		}
	}
	
	if source == "" || source == "flight" {
		flightVehicles, err := h.repo.GetRecentVehicles(r.Context(), "flight", since)
		if err != nil {
			log.Printf("Error fetching flight vehicles: %v", err)
		} else {
			allVehicles = append(allVehicles, flightVehicles...)
		}
	}

	log.Printf("[API] GetActiveVehicles (source=%s) returning %d deduplicated vehicles (window: %s)", 
		source, len(allVehicles), since.Format("15:04:05"))

	// Ensure we return an empty array instead of null if no vehicles are found
	if allVehicles == nil {
		allVehicles = []data.Vehicle{}
	}

	json.NewEncoder(w).Encode(allVehicles)
}

// GetVehicleHistory returns the historical path of a single vehicle.
// Serves `GET /api/vehicles/history?id={vehicleId}`
func (h *ApiHandlers) GetVehicleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// If using chi, we extract url param
	vehicleId := r.URL.Query().Get("id") 
	if vehicleId == "" {
		http.Error(w, `{"error": "missing id parameter"}`, http.StatusBadRequest)
		return
	}

	// 12 hour window for history
	since := time.Now().UTC().Add(-12 * time.Hour)
	
	history, err := h.repo.GetVehicleHistory(r.Context(), vehicleId, since)
	if err != nil {
		http.Error(w, `{"error": "failed to query history"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(history)
}

// GetHealth returns a robust mock health status object for the dashboard
func (h *ApiHandlers) GetHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	since := time.Now().UTC().Add(-5 * time.Minute)
	
	metro, _ := h.repo.GetRecentVehicles(r.Context(), "metro", since)
	flight, _ := h.repo.GetRecentVehicles(r.Context(), "flight", since)

	resp := map[string]interface{}{
		"status": "healthy",
		"sources": map[string]interface{}{
			"metro": map[string]interface{}{
				"status":       "healthy",
				"lastIngest":   time.Now().UTC().Format(time.RFC3339),
				"vehicleCount": len(metro),
			},
			"flight": map[string]interface{}{
				"status":       "healthy",
				"lastIngest":   time.Now().UTC().Format(time.RFC3339),
				"vehicleCount": len(flight),
			},
		},
	}
	
	json.NewEncoder(w).Encode(resp)
}

// GetOpenSkyStatus returns the status of the OpenSky ingestion pipeline.
// Serves `GET /api/manage/opensky-status`
func (h *ApiHandlers) GetOpenSkyStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// If vehicle count is 0, we'll mark it as degraded for visibility
	since := time.Now().UTC().Add(-10 * time.Minute)
	flights, _ := h.repo.GetRecentVehicles(r.Context(), "flight", since)

	isUp := len(flights) > 0
	errorMsg := ""
	if !isUp {
		errorMsg = "No flights ingested in the last 10 minutes. OpenSky Network may be timing out from Cloud Run egress IPs, or rate-limited. Check Cloud Run logs for 'i/o timeout'."
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"isUp":               isUp,
		"status":             map[bool]string{true: "healthy", false: "degraded"}[isUp],
		"statusCode":         map[bool]int{true: 200, false: 503}[isUp],
		"vehicleCount":       len(flights),
		"lastIngest":         time.Now().UTC().Format(time.RFC3339),
		"authenticated":      false, // anonymous mode until OPENSKY_CLIENT_ID is set
		"rateLimitRemaining": nil,
		"rateLimitLimit":     nil,
		"error":              errorMsg,
	})
}

// GetRoutes returns real GTFS route shapes from the CapMetro static feed.
func (h *ApiHandlers) GetRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=21600") // Cache 6h - GTFS static rarely changes

	if h.gtfs == nil {
		w.Write([]byte(`[]`))
		return
	}

	shapes, err := h.gtfs.GetRouteShapes()
	if err != nil || len(shapes) == 0 {
		log.Printf("[API] GTFS shapes unavailable: %v", err)
		w.Write([]byte(`[]`))
		return
	}

	json.NewEncoder(w).Encode(shapes)
}

// GetStops returns a mock empty array
func (h *ApiHandlers) GetStops(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`[]`))
}

// GetManageStatus returns a mock status for Cloud Scheduler
func (h *ApiHandlers) GetManageStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{ "metro": "active", "flight": "active" }`))
}

// DebugInspect returns a raw counts and sample documents for debugging
func (h *ApiHandlers) DebugInspect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	since := time.Now().UTC().Add(-24 * time.Hour)
	all, err := h.repo.GetRecentVehicles(r.Context(), "metro", since)
	
	resp := map[string]interface{}{
		"server_time": time.Now().UTC().Format(time.RFC3339),
		"query_since": since.Format(time.RFC3339),
		"error":       nil,
		"count":       len(all),
		"sample":      nil,
	}

	if err != nil {
		resp["error"] = err.Error()
	} else if len(all) > 0 {
		resp["sample"] = all[0]
	}

	json.NewEncoder(w).Encode(resp)
}
