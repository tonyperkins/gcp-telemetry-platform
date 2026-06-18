package services

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CapMetro's GTFS static feed via Austin Open Data Portal
// BlobId discovered via: curl data.austintexas.gov/api/views/r4v4-vz24.json | jq .blobId
const capMetroGtfsUrl = "https://data.austintexas.gov/api/views/r4v4-vz24/files/019ddc54-7427-49fb-91c7-fbe1cf496af7?download=true&filename=capmetro.zip"

// RouteShapeData is returned by GetRouteShapes
type RouteShapeData struct {
	RouteID    string       `json:"routeId"`
	ShortName  string       `json:"shortName"`
	LongName   string       `json:"longName"`
	Color      string       `json:"color"`
	Directions []ShapeDir   `json:"directions"`
}

type ShapeDir struct {
	DirectionID int         `json:"directionId"`
	Shape       [][2]float64 `json:"shape"` // [lat, lon] pairs
}

// GtfsShapeService fetches and caches route shape data from GTFS static
type GtfsShapeService struct {
	client     *http.Client
	mu         sync.RWMutex
	shapes     []RouteShapeData
	lastFetch  time.Time
	cacheTTL   time.Duration
}

func NewGtfsShapeService(client *http.Client) *GtfsShapeService {
	return &GtfsShapeService{
		client:   client,
		cacheTTL: 6 * time.Hour,
	}
}

// GetRouteShapes returns cached route shapes, refreshing if stale
func (g *GtfsShapeService) GetRouteShapes() ([]RouteShapeData, error) {
	g.mu.RLock()
	if len(g.shapes) > 0 && time.Since(g.lastFetch) < g.cacheTTL {
		shapes := g.shapes
		g.mu.RUnlock()
		return shapes, nil
	}
	g.mu.RUnlock()

	// Need to refresh
	shapes, err := g.fetchGtfsShapes()
	if err != nil {
		log.Printf("[GTFS] Failed to fetch shapes: %v", err)
		// Return stale cache rather than empty
		g.mu.RLock()
		defer g.mu.RUnlock()
		return g.shapes, nil
	}

	g.mu.Lock()
	g.shapes = shapes
	g.lastFetch = time.Now()
	g.mu.Unlock()

	log.Printf("[GTFS] Loaded %d route shapes", len(shapes))
	return shapes, nil
}

func (g *GtfsShapeService) fetchGtfsShapes() ([]RouteShapeData, error) {
	log.Printf("[GTFS] Fetching GTFS static feed from CapMetro...")

	req, err := http.NewRequest("GET", capMetroGtfsUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Austin Telemetry Platform)")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse ZIP in memory
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}

	// Read routes.txt and shapes.txt from the ZIP
	routeMap := make(map[string]RouteShapeData)
	shapePoints := make(map[string][][2]float64) // shapeId -> sorted points

	for _, f := range zr.File {
		switch strings.ToLower(f.Name) {
		case "routes.txt":
			rc, err := f.Open()
			if err != nil {
				continue
			}
			parseRoutes(rc, routeMap)
			rc.Close()
		case "shapes.txt":
			rc, err := f.Open()
			if err != nil {
				continue
			}
			parseShapePoints(rc, shapePoints)
			rc.Close()
		}
	}

	// Link trips.txt to get shape_id -> route_id mapping
	tripShapeMap := make(map[string][]string) // routeId -> []shapeId
	for _, f := range zr.File {
		if strings.ToLower(f.Name) == "trips.txt" {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			parseTrips(rc, tripShapeMap)
			rc.Close()
			break
		}
	}

	// Build final RouteShapeData
	result := make([]RouteShapeData, 0, len(routeMap))
	for routeId, rsd := range routeMap {
		shapeIds := tripShapeMap[routeId]
		seen := make(map[string]bool)
		var dirs []ShapeDir
		for i, sid := range shapeIds {
			if seen[sid] {
				continue
			}
			seen[sid] = true
			pts := shapePoints[sid]
			if len(pts) == 0 {
				continue
			}
			dirs = append(dirs, ShapeDir{DirectionID: i % 2, Shape: pts})
		}
		rsd.Directions = dirs
		result = append(result, rsd)
	}

	return result, nil
}

func parseRoutes(r io.Reader, routeMap map[string]RouteShapeData) {
	cr := csv.NewReader(r)
	cr.LazyQuotes = true
	headers, err := cr.Read()
	if err != nil {
		return
	}
	idx := headerIndex(headers)

	for {
		rec, err := cr.Read()
		if err != nil {
			break
		}
		rid := safeGet(rec, idx["route_id"])
		if rid == "" {
			continue
		}
		color := safeGet(rec, idx["route_color"])
		if color != "" && !strings.HasPrefix(color, "#") {
			color = "#" + color
		}
		if color == "" {
			color = "#3B82F6"
		}
		routeMap[rid] = RouteShapeData{
			RouteID:    rid,
			ShortName:  safeGet(rec, idx["route_short_name"]),
			LongName:   safeGet(rec, idx["route_long_name"]),
			Color:      color,
			Directions: []ShapeDir{},
		}
	}
}

func parseShapePoints(r io.Reader, shapePoints map[string][][2]float64) {
	type rawPt struct {
		lat, lon float64
		seq      int
	}
	rawMap := make(map[string][]rawPt)

	cr := csv.NewReader(r)
	cr.LazyQuotes = true
	headers, err := cr.Read()
	if err != nil {
		return
	}
	idx := headerIndex(headers)

	for {
		rec, err := cr.Read()
		if err != nil {
			break
		}
		sid := safeGet(rec, idx["shape_id"])
		lat, _ := strconv.ParseFloat(safeGet(rec, idx["shape_pt_lat"]), 64)
		lon, _ := strconv.ParseFloat(safeGet(rec, idx["shape_pt_lon"]), 64)
		seq, _ := strconv.Atoi(safeGet(rec, idx["shape_pt_sequence"]))
		if sid == "" || lat == 0 {
			continue
		}
		rawMap[sid] = append(rawMap[sid], rawPt{lat, lon, seq})
	}

	for sid, pts := range rawMap {
		sort.Slice(pts, func(i, j int) bool { return pts[i].seq < pts[j].seq })
		coords := make([][2]float64, len(pts))
		for i, p := range pts {
			coords[i] = [2]float64{p.lat, p.lon}
		}
		shapePoints[sid] = coords
	}
}

func parseTrips(r io.Reader, tripShapeMap map[string][]string) {
	cr := csv.NewReader(r)
	cr.LazyQuotes = true
	headers, err := cr.Read()
	if err != nil {
		return
	}
	idx := headerIndex(headers)
	seen := make(map[string]map[string]bool)

	for {
		rec, err := cr.Read()
		if err != nil {
			break
		}
		rid := safeGet(rec, idx["route_id"])
		sid := safeGet(rec, idx["shape_id"])
		if rid == "" || sid == "" {
			continue
		}
		if seen[rid] == nil {
			seen[rid] = make(map[string]bool)
		}
		if !seen[rid][sid] {
			seen[rid][sid] = true
			tripShapeMap[rid] = append(tripShapeMap[rid], sid)
		}
	}
}

func headerIndex(headers []string) map[string]int {
	m := make(map[string]int, len(headers))
	for i, h := range headers {
		m[strings.TrimSpace(h)] = i
	}
	return m
}

func safeGet(rec []string, idx int) string {
	if idx < 0 || idx >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[idx])
}
