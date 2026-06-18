package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
)

const (
	// Primary: official API endpoint
	openSkyBaseUrl = "https://api.opensky-network.org/api/states/all"
	// Fallback: naked domain version
	openSkyFallback   = "https://opensky-network.org/api/states/all"
	openSkyAuthUrl    = "https://auth.opensky-network.org/auth/realms/opensky-network/protocol/openid-connect/token"
	rateLimitCooldown = 5 * time.Minute
)

// FlightIngestionService handles fetching and parsing OpenSky network data.
type FlightIngestionService struct {
	client            *http.Client
	repo              *data.FirestoreRepository
	clientId          string
	clientSecret      string
	cachedToken       string
	tokenExpiry       time.Time
	lastRateLimitTime *time.Time
}

// NewFlightIngestionService creates a new instance.
func NewFlightIngestionService(client *http.Client, repo *data.FirestoreRepository, clientId, clientSecret string) *FlightIngestionService {
	if clientId != "" && clientId != "pending" && clientSecret != "" && clientSecret != "pending" {
		log.Printf("OpenSky OAuth2 mode enabled (4000 credits/day). User: %s...", clientId[:3])
	} else {
		log.Printf("OpenSky running in anonymous mode (400 credits/day). Missing OPENSKY_CLIENT_ID or SECRET.")
	}

	return &FlightIngestionService{
		client:       client,
		repo:         repo,
		clientId:     clientId,
		clientSecret: clientSecret,
	}
}

// FetchAndSave fetches vehicles from OpenSky and saves them to Firestore.
func (s *FlightIngestionService) FetchAndSave(ctx context.Context, bboxConfig string) error {
	vehicles, err := s.fetchOpenSky(ctx, bboxConfig)
	if err != nil {
		return fmt.Errorf("failed to fetch OpenSky vehicles: %w", err)
	}

	if len(vehicles) == 0 {
		return nil
	}

	if err := s.repo.SaveVehicles(ctx, vehicles); err != nil {
		return fmt.Errorf("failed to save OpenSky vehicles: %w", err)
	}

	log.Printf("Ingested %d aircraft", len(vehicles))
	return nil
}

// Fetch retrieves and parses current aircraft from OpenSky without persisting.
// Used by the off-cloud pusher (which runs from a residential IP that OpenSky
// does not block) to obtain flights before forwarding them to the push endpoint.
func (s *FlightIngestionService) Fetch(ctx context.Context, bboxConfig string) ([]data.Vehicle, error) {
	return s.fetchOpenSky(ctx, bboxConfig)
}

// SaveExternal persists flight vehicles received from an external (off-cloud)
// pusher. It normalizes the source and ingest timestamp before writing.
func (s *FlightIngestionService) SaveExternal(ctx context.Context, vehicles []data.Vehicle) (int, error) {
	if len(vehicles) == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	for i := range vehicles {
		vehicles[i].Source = "flight"
		if vehicles[i].IngestedAt.IsZero() {
			vehicles[i].IngestedAt = now
		}
	}
	if err := s.repo.SaveVehicles(ctx, vehicles); err != nil {
		return 0, err
	}
	return len(vehicles), nil
}

func (s *FlightIngestionService) fetchOpenSky(ctx context.Context, bboxConfig string) ([]data.Vehicle, error) {
	if s.lastRateLimitTime != nil {
		if time.Since(*s.lastRateLimitTime) < rateLimitCooldown {
			log.Printf("OpenSky circuit breaker active. Skipping call.")
			return nil, nil
		}
		log.Printf("OpenSky circuit breaker reset. Resuming API calls.")
		s.lastRateLimitTime = nil
	}

	token, _ := s.getAuthToken(ctx)

	reqUrl, err := buildUrl(bboxConfig)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", reqUrl, nil)
	if err != nil {
		return nil, err
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// Try fallback URL if primary fails
		log.Printf("OpenSky primary URL failed (%v), trying fallback...", err)
		fallbackUrl := strings.Replace(reqUrl, openSkyBaseUrl, openSkyFallback, 1)
		req2, _ := http.NewRequestWithContext(ctx, "GET", fallbackUrl, nil)
		if req2 != nil {
			if token != "" {
				req2.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err = s.client.Do(req2)
		}
		if err != nil {
			return nil, fmt.Errorf("request failed (both endpoints): %w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		now := time.Now()
		s.lastRateLimitTime = &now
		log.Printf("OpenSky rate limit exceeded (429). Activating circuit breaker.")
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenSky returned %v", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	return s.parseResponse(body)
}

func (s *FlightIngestionService) parseResponse(body []byte) ([]data.Vehicle, error) {
	var root struct {
		States [][]interface{} `json:"states"`
	}

	if err := json.Unmarshal(body, &root); err != nil {
		// Log missing states array as an SRE warning
		log.Printf("OpenSky response missing 'states' array. Root JSON: %s", string(body))
		return nil, nil
	}

	var vehicles []data.Vehicle
	now := time.Now().UTC()
	rawBodyStr := string(body)

	for _, state := range root.States {
		if len(state) < 11 {
			continue
		}

		vehicle := data.Vehicle{
			Source:     "flight",
			VehicleID:  getString(state[0]),
			Label:      strings.TrimSpace(getString(state[1])),
			Longitude:  getFloat(state[5]),
			Latitude:   getFloat(state[6]),
			AltitudeM:  getFloatPtr(state[7]),
			OnGround:   getBoolPtr(state[8]),
			SpeedKmh:   getSpeedKmh(getFloatPtr(state[9])), // Velocity is in m/s, convert to km/h
			Heading:    getFloatPtr(state[10]),
			IngestedAt: now,
			RawJSON:    rawBodyStr,
		}

		vehicles = append(vehicles, vehicle)
	}

	return vehicles, nil
}

func buildUrl(bboxConfig string) (string, error) {
	parts := strings.Split(bboxConfig, ",")
	if len(parts) != 4 {
		return "", fmt.Errorf("OPENSKY_BBOX must be 'lamin,lomin,lamax,lomax'")
	}
	// Minimal trimming, Go ParseFloat handles formatting natively
	return fmt.Sprintf("%s?lamin=%s&lomin=%s&lamax=%s&lomax=%s",
		openSkyBaseUrl, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]),
		strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])), nil
}

func (s *FlightIngestionService) getAuthToken(ctx context.Context) (string, error) {
	if s.cachedToken != "" && time.Now().Before(s.tokenExpiry) {
		return s.cachedToken, nil
	}

	if s.clientId == "" || s.clientSecret == "" {
		return "", nil
	}

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", s.clientId)
	data.Set("client_secret", s.clientSecret)

	req, err := http.NewRequestWithContext(ctx, "POST", openSkyAuthUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		if token, ok := result["access_token"].(string); ok {
			s.cachedToken = token
			expiresIn := 1800
			if exp, ok := result["expires_in"].(float64); ok {
				expiresIn = int(exp)
			}
			s.tokenExpiry = time.Now().Add(time.Duration(expiresIn-60) * time.Second)
			log.Printf("OpenSky OAuth2 token refreshed.")
			return token, nil
		}
	}
	return "", fmt.Errorf("failed to fetch auth token, status: %d", resp.StatusCode)
}

// Helpers

func getString(val interface{}) string {
	if s, ok := val.(string); ok {
		return s
	}
	return ""
}

func getFloat(val interface{}) float64 {
	if f, ok := val.(float64); ok {
		return f
	}
	return 0
}

func getFloatPtr(val interface{}) *float64 {
	if f, ok := val.(float64); ok {
		return &f
	}
	return nil
}

func getBoolPtr(val interface{}) *bool {
	if b, ok := val.(bool); ok {
		return &b
	}
	return nil
}

func getSpeedKmh(velocityMs *float64) *float64 {
	if velocityMs == nil {
		return nil
	}
	kmh := *velocityMs * 3.6
	return &kmh
}
