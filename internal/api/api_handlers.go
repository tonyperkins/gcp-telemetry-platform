package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/services"
)

// ApiHandlers exposes the REST API for the Glance dashboard / frontend.
type ApiHandlers struct {
	repo *data.FirestoreRepository
	gtfs *services.GtfsShapeService
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

// GetVehiclePaths returns historical position trails for all vehicles of a
// given source within a time window, grouped by vehicle_id.
// Serves `GET /api/vehicles/paths?source={source}&minutes={minutes}`
func (h *ApiHandlers) GetVehiclePaths(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	source := r.URL.Query().Get("source")
	if source != "metro" && source != "flight" {
		http.Error(w, `{"error": "invalid or missing source parameter"}`, http.StatusBadRequest)
		return
	}

	minutes := 20
	if m := r.URL.Query().Get("minutes"); m != "" {
		if val, err := strconv.Atoi(m); err == nil && val > 0 && val <= 720 {
			minutes = val
		}
	}

	since := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute)

	paths, err := h.repo.GetVehiclePaths(r.Context(), source, since)
	if err != nil {
		log.Printf("[API] Error fetching vehicle paths: %v", err)
		http.Error(w, `{"error": "failed to query paths"}`, http.StatusInternalServerError)
		return
	}

	if paths == nil {
		paths = []data.VehiclePathGroup{}
	}

	log.Printf("[API] GetVehiclePaths (source=%s, minutes=%d) returning %d vehicle trails",
		source, minutes, len(paths))

	json.NewEncoder(w).Encode(paths)
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
				"lastIngest":   latestIngest(metro),
				"vehicleCount": len(metro),
			},
			"flight": map[string]interface{}{
				"status":       "healthy",
				"lastIngest":   latestIngest(flight),
				"vehicleCount": len(flight),
			},
		},
	}

	json.NewEncoder(w).Encode(resp)
}

// latestIngest returns the most recent IngestedAt across the vehicles as an
// RFC3339 string, or "" if none. This reflects the true age of the data rather
// than the time the health endpoint was polled.
func latestIngest(vehicles []data.Vehicle) string {
	var latest time.Time
	for _, v := range vehicles {
		if v.IngestedAt.After(latest) {
			latest = v.IngestedAt
		}
	}
	if latest.IsZero() {
		return ""
	}
	return latest.UTC().Format(time.RFC3339)
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

// logSourceKeywords maps a UI log "source" to the case-insensitive substrings used
// to filter lines out of the single combined server log file. All server logs are
// written to one file; the source tabs in the dashboard are filtered views of it.
var logSourceKeywords = map[string][]string{
	"metro":  {"metro", "gtfs", "capmetro"},
	"flight": {"flight", "aircraft", "opensky", "flightpusher"},
	"api":    {"[api]"},
}

// matchesLogSource reports whether a log line belongs to the given source.
// A source with no keyword mapping (e.g. "all"/"dashboard") shows every line.
func matchesLogSource(line, source string) bool {
	keywords, ok := logSourceKeywords[source]
	if !ok {
		// Catch-all: show everything.
		return true
	}
	lower := strings.ToLower(line)
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// GetLogs serves the last N lines of the server log file, filtered by source.
// All server components log to the same file; the source param selects a filtered
// view (metro / flight / api / dashboard) rather than a separate file.
func (h *ApiHandlers) GetLogs(w http.ResponseWriter, r *http.Request) {
	source := chi.URLParam(r, "source")

	lines := r.URL.Query().Get("lines")
	maxLines, err := strconv.Atoi(lines)
	if err != nil || maxLines <= 0 || maxLines > 1000 {
		maxLines = 200
	}

	logFilePath := os.Getenv("LOG_FILE_PATH")
	if logFilePath == "" {
		logFilePath = "/tmp/logs/out.log"
	}

	// Read a larger raw window so that after filtering by source we still have a
	// useful number of lines to show.
	rawLines, err := readLastLines(logFilePath, maxLines*20)
	if err != nil {
		log.Printf("[API] Failed to read log file %s: %v", logFilePath, err)
		http.Error(w, `{"error": "failed to read logs"}`, http.StatusInternalServerError)
		return
	}

	filtered := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if matchesLogSource(line, source) {
			filtered = append(filtered, line)
		}
	}

	// Keep only the most recent maxLines after filtering.
	totalMatched := len(filtered)
	if len(filtered) > maxLines {
		filtered = filtered[len(filtered)-maxLines:]
	}

	filename := filepath.Base(logFilePath)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"filename":   filename,
		"lines":      filtered,
		"count":      len(filtered),
		"totalLines": totalMatched,
	})
}

// readLastLines reads the last n lines from the named file.
func readLastLines(filename string, n int) ([]string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	const blockSize = 4096
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return []string{}, nil
	}

	// Read the file in reverse chunks until we have enough lines.
	buf := make([]byte, 0, blockSize)
	cursor := info.Size()
	lines := make([]string, 0, n)

	for cursor > 0 && len(lines) < n {
		readSize := int64(blockSize)
		if cursor < readSize {
			readSize = cursor
		}
		cursor -= readSize

		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, cursor); err != nil {
			return nil, err
		}
		buf = append(chunk, buf...)

		for len(buf) > 0 && len(lines) < n {
			idx := len(buf) - 1
			for idx > 0 && buf[idx-1] != '\n' {
				idx--
			}
			line := string(buf[idx:])
			if line != "" {
				lines = append(lines, line)
			}
			buf = buf[:idx]
		}
	}

	// Reverse lines so they are oldest-to-newest.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}
