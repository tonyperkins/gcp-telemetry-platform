package api

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/services"
)

// maxPushBytes caps the push request body to guard against oversized payloads.
const maxPushBytes = 5 << 20 // 5 MiB

// WorkerHandlers exposes HTTP endpoints meant to be invoked by GCP Cloud Scheduler.
type WorkerHandlers struct {
	flightService *services.FlightIngestionService
	metroService  *services.MetroIngestionService
}

func NewWorkerHandlers(flight *services.FlightIngestionService, metro *services.MetroIngestionService) *WorkerHandlers {
	return &WorkerHandlers{
		flightService: flight,
		metroService:  metro,
	}
}

// HandleFlightIngest is triggered by Cloud Scheduler (e.g. every minute)
func (h *WorkerHandlers) HandleFlightIngest(w http.ResponseWriter, r *http.Request) {
	// Simple auth check: ensure it came from Cloud Scheduler
	// In production, validate the OIDC token sent by Cloud Scheduler
	if r.Header.Get("X-CloudScheduler") != "true" && os.Getenv("ENV") != "dev" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	bbox := os.Getenv("OPENSKY_BBOX")
	if bbox == "" {
		bbox = "29.8,-98.2,30.8,-97.2" // default Austin fallback
	}

	if err := h.flightService.FetchAndSave(r.Context(), bbox); err != nil {
		log.Printf("Flight ingestion failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// HandleFlightPush ingests flight data pushed by an off-cloud worker.
//
// OpenSky blocks GCP datacenter egress, so flights cannot be fetched from Cloud
// Run directly. Instead, a pusher running on a non-cloud (residential) IP fetches
// OpenSky and POSTs the parsed vehicles here. Authenticated with a bearer token
// (INGEST_PUSH_TOKEN) since this endpoint is publicly reachable and writes to the
// datastore. The payload is public flight telemetry, so a shared secret over TLS
// is an appropriate guard.
func (h *WorkerHandlers) HandleFlightPush(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("INGEST_PUSH_TOKEN")
	if os.Getenv("ENV") != "dev" {
		if token == "" || token == "pending" {
			http.Error(w, "Push endpoint not configured", http.StatusNotImplemented)
			return
		}
		expected := "Bearer " + token
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var vehicles []data.Vehicle
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPushBytes))
	if err := dec.Decode(&vehicles); err != nil {
		http.Error(w, "Invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	count, err := h.flightService.SaveExternal(r.Context(), vehicles)
	if err != nil {
		log.Printf("Flight push save failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Ingested %d aircraft (pushed)", count)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"ingested": count})
}

// HandleMetroIngest is triggered by Cloud Scheduler (e.g. every 30 seconds)
func (h *WorkerHandlers) HandleMetroIngest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-CloudScheduler") != "true" && os.Getenv("ENV") != "dev" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	feedUrl := os.Getenv("METRO_GTFS_RT_URL")
	if feedUrl == "" {
		feedUrl = "https://data.texas.gov/download/eiei-9rpf/application/octet-stream"
	}

	if err := h.metroService.FetchAndSave(r.Context(), feedUrl); err != nil {
		log.Printf("Metro ingestion failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
