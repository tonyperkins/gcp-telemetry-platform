// Command flightpusher runs off-cloud (e.g. on a home Docker box with a
// residential IP) to work around OpenSky blocking GCP datacenter egress.
//
// It fetches current aircraft from OpenSky using the same parsing logic as the
// main server, then POSTs the parsed vehicles to the Cloud Run push endpoint
// (/ingest/flight/push), authenticated with INGEST_PUSH_TOKEN. The Cloud Run
// service writes them to Firestore, so the dashboard renders flights normally.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/services"
)

func main() {
	pushURL := mustEnv("PUSH_URL") // e.g. https://telemetry-api-xxxx.run.app/ingest/flight/push
	token := mustEnv("INGEST_PUSH_TOKEN")

	bbox := os.Getenv("OPENSKY_BBOX")
	if bbox == "" {
		bbox = "29.8,-98.2,30.8,-97.2" // Austin default
	}

	interval := 60 * time.Second
	if v := os.Getenv("POLL_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	runOnce := os.Getenv("RUN_ONCE") == "true"

	httpClient := &http.Client{Timeout: 60 * time.Second}
	// nil repo: the pusher only uses Fetch (no Firestore access on the home box).
	flightService := services.NewFlightIngestionService(
		httpClient, nil,
		os.Getenv("OPENSKY_CLIENT_ID"), os.Getenv("OPENSKY_CLIENT_SECRET"),
	)

	push := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		vehicles, err := flightService.Fetch(ctx, bbox)
		if err != nil {
			log.Printf("fetch failed: %v", err)
			return
		}
		if len(vehicles) == 0 {
			log.Printf("no aircraft returned (quiet airspace or circuit breaker); nothing to push")
			return
		}

		body, err := json.Marshal(vehicles)
		if err != nil {
			log.Printf("marshal failed: %v", err)
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("request build failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("push failed: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("push rejected: HTTP %d", resp.StatusCode)
			return
		}
		log.Printf("pushed %d aircraft to %s", len(vehicles), pushURL)
	}

	log.Printf("flightpusher started (bbox=%s, interval=%s, runOnce=%t)", bbox, interval, runOnce)
	push()
	if runOnce {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		push()
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}
