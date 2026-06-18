package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/api"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/data"
	"github.com/tonyperkins/gcp-telemetry-platform/internal/services"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx := context.Background()

	// 1. Initialize Firestore Client
	dbID := os.Getenv("FIRESTORE_DB_ID")
	if dbID == "" {
		dbID = "(default)" // fallback to the literal string (default)
	}
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		projectID = "gcp-telemetry-platform"
	}

	firestoreClient, err := firestore.NewClientWithDatabase(ctx, projectID, dbID)
	if err != nil && os.Getenv("ENV") != "dev" {
		log.Fatalf("Error initializing firestore client: %v\n", err)
	}

	// Create a dummy client for dev if no credentials provided
	var repo *data.FirestoreRepository
	if firestoreClient != nil {
		repo = data.NewFirestoreRepository(firestoreClient)
	} else {
		log.Println("WARNING: Running with nil firestore client (dev mode)")
		// In a real scenario we'd use a mock, but for bridging we'll let it panic if hit
	}

	// 2. Initialize Services
	httpClient := &http.Client{Timeout: 60 * time.Second}
	openSkyClientId := os.Getenv("OPENSKY_CLIENT_ID")
	openSkySecret := os.Getenv("OPENSKY_CLIENT_SECRET")

	flightService := services.NewFlightIngestionService(httpClient, repo, openSkyClientId, openSkySecret)
	metroService := services.NewMetroIngestionService(httpClient, repo)

	// 3. Initialize Handlers
	gtfsService := services.NewGtfsShapeService(httpClient)
	workerHandlers := api.NewWorkerHandlers(flightService, metroService)
	apiHandlers := api.NewApiHandlers(repo, gtfsService)

	mgmtHandlers, err := api.NewManagementHandlers(ctx)
	if err != nil {
		log.Printf("Warning: Failed to init management handlers (scheduler control): %v", err)
	}

	// 4. Router Setup
	r := chi.NewRouter()

	// Basic CORS
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("healthy"))
	})

	// Public Telemetry API
	r.Get("/api/health", apiHandlers.GetHealth)

	r.Route("/api/vehicles", func(r chi.Router) {
		r.Get("/current", apiHandlers.GetActiveVehicles)
		r.Get("/history", apiHandlers.GetVehicleHistory)
	})

	r.Route("/api/routes", func(r chi.Router) {
		r.Get("/", apiHandlers.GetRoutes)
		r.Get("/stops/all", apiHandlers.GetStops)
	})

	r.Route("/api/manage", func(r chi.Router) {
		r.Get("/status", apiHandlers.GetManageStatus)
		r.Get("/opensky-status", apiHandlers.GetOpenSkyStatus)
		// Mock heartbeat to keep the frontend console clean
		r.Post("/heartbeat", func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		r.Post("/start", func(w http.ResponseWriter, req *http.Request) {
			if mgmtHandlers == nil {
				http.Error(w, "Management disabled", http.StatusNotImplemented)
				return
			}
			mgmtHandlers.HandleStart(w, req)
		})
		r.Post("/stop", func(w http.ResponseWriter, req *http.Request) {
			if mgmtHandlers == nil {
				http.Error(w, "Management disabled", http.StatusNotImplemented)
				return
			}
			mgmtHandlers.HandleStop(w, req)
		})
	})

	r.Get("/api/debug/inspect", apiHandlers.DebugInspect)

	// Internal Worker Triggers (invoked purely by Cloud Scheduler)
	r.Route("/ingest", func(r chi.Router) {
		r.Post("/flight", workerHandlers.HandleFlightIngest)
		r.Post("/metro", workerHandlers.HandleMetroIngest)
		// Authenticated push from the off-cloud flight pusher (OpenSky blocks GCP egress)
		r.Post("/flight/push", workerHandlers.HandleFlightPush)
	})

	// Serve the React frontend (Demo At Rest - Single Container Architecture)
	fileServer := http.FileServer(http.Dir("./dashboard/dist"))
	r.Handle("/*", http.StripPrefix("/", fileServer))

	log.Printf("Server listening on port %s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
