variable "project_id" {
  type        = string
  description = "The GCP project ID"
}

variable "region" {
  type        = string
  default     = "us-central1"
  description = "The GCP region to deploy resources to"
}

variable "image_url" {
  type        = string
  description = "The URL of the Docker image in Artifact Registry or Docker Hub"
}

variable "opensky_bbox" {
  type        = string
  description = "Bounding box for OpenSky tracking (lamin,lomin,lamax,lomax)"
  default     = "29.8,-98.2,30.8,-97.2"
}

variable "flight_polling_cron" {
  type        = string
  description = "Cron expression for OpenSky API fetching"
  default     = "*/1 * * * *"
}
