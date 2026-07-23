variable "project_id" {
  type        = string
  description = "GCP project ID where all resources are deployed. Must exist before applying."

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid GCP project ID (lowercase, 6-30 chars, alphanumeric + hyphens)."
  }
}

variable "region" {
  type        = string
  default     = "us-central1"
  description = "GCP region for Cloud Run and Cloud Scheduler. deploy.sh/destroy.sh override this to europe-west3."
}

variable "image_url" {
  type        = string
  description = "Full URI of the container image in Artifact Registry (e.g. europe-west3-docker.pkg.dev/PROJECT/telemetry-repo/telemetry-app:TAG)."
}

variable "opensky_bbox" {
  type        = string
  description = "Bounding box for OpenSky flight tracking: lamin,lomin,lamax,lomax. Default covers Austin, TX."
  default     = "29.8,-98.2,30.8,-97.2"
}

variable "metro_polling_cron" {
  type        = string
  description = "Cron expression for the Metro GTFS-RT poll (Cloud Scheduler). Default every 2 min keeps Cloud Run invocations and Firestore writes within the free tier."
  default     = "*/2 * * * *"
}
