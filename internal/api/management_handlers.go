package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
)

// ManagementHandlers exposes endpoints to start/stop the remote workers.
type ManagementHandlers struct {
	schedulerClient *scheduler.CloudSchedulerClient
	projectID       string
	locationID      string
	metroJobName    string
	flightJobName   string
}

func NewManagementHandlers(ctx context.Context) (*ManagementHandlers, error) {
	client, err := scheduler.NewCloudSchedulerClient(ctx)
	if err != nil && os.Getenv("ENV") != "dev" {
		return nil, fmt.Errorf("failed to init scheduler client: %w", err)
	}

	projectID := os.Getenv("GCP_PROJECT_ID")
	locationID := os.Getenv("GCP_LOCATION") // e.g. us-central1

	if projectID == "" {
		projectID = "demo-project"
	}
	if locationID == "" {
		locationID = "us-central1"
	}

	return &ManagementHandlers{
		schedulerClient: client,
		projectID:       projectID,
		locationID:      locationID,
		metroJobName:    fmt.Sprintf("projects/%s/locations/%s/jobs/metro-ingester", projectID, locationID),
		flightJobName:   fmt.Sprintf("projects/%s/locations/%s/jobs/flight-ingester", projectID, locationID),
	}, nil
}

// HandleStart ingestion triggers the Cloud Scheduler to RESUME.
func (h *ManagementHandlers) HandleStart(w http.ResponseWriter, r *http.Request) {
	// Dummy token guard for demo purposes. In production, validate an IAM/OIDC identity token.
	if r.Header.Get("Authorization") != "Bearer "+os.Getenv("MANAGEMENT_ADMIN_TOKEN") && os.Getenv("ENV") != "dev" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Resume Metro
	_, err := h.schedulerClient.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: h.metroJobName})
	if err != nil {
		log.Printf("Failed to resume metro job: %v", err)
	}

	// Resume Flight
	_, err = h.schedulerClient.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: h.flightJobName})
	if err != nil {
		log.Printf("Failed to resume flight job: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status": "ingestion_started"}`))
}

// HandleStop ingestion triggers the Cloud Scheduler to PAUSE.
func (h *ManagementHandlers) HandleStop(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+os.Getenv("MANAGEMENT_ADMIN_TOKEN") && os.Getenv("ENV") != "dev" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Pause Metro
	_, err := h.schedulerClient.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: h.metroJobName})
	if err != nil {
		log.Printf("Failed to pause metro job: %v", err)
	}

	// Pause Flight
	_, err = h.schedulerClient.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: h.flightJobName})
	if err != nil {
		log.Printf("Failed to pause flight job: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status": "ingestion_stopped"}`))
}
