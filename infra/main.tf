terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
  }
  # Remote state backend is configured in backend.tf (GCS bucket).
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# ---------------------------------------------------------------------------
# Egress note
# ---------------------------------------------------------------------------
# This service previously ran a VPC connector + Cloud NAT + reserved static IP
# purely to give OpenSky a stable egress IP. OpenSky blocks GCP datacenter IP
# ranges regardless of region or static IP (see README "Known limitations"),
# so that stack was pure cost (~$45/mo) for zero benefit. Flights now arrive
# via the off-cloud flight pusher (POST /ingest/flight/push), and the Capital
# Metro feed is reachable over Cloud Run's default direct internet egress, so
# the entire VPC/NAT stack has been removed.

# ---------------------------------------------------------------------------
# Secret Manager for OpenSky Credentials + flight push token
# ---------------------------------------------------------------------------
data "google_secret_manager_secret" "opensky_client_id" {
  secret_id = "OPENSKY_CLIENT_ID"
}

data "google_secret_manager_secret" "opensky_client_secret" {
  secret_id = "OPENSKY_CLIENT_SECRET"
}

# Shared bearer token guarding the off-cloud flight push endpoint.
data "google_secret_manager_secret" "ingest_push_token" {
  secret_id = "INGEST_PUSH_TOKEN"
}

# ---------------------------------------------------------------------------
# Service Account for Cloud Run (Identity)
# ---------------------------------------------------------------------------
resource "google_service_account" "cloud_run_sa" {
  account_id   = "telemetry-cloudrun-sa"
  display_name = "Telemetry Platform Cloud Run Service Account"
}

# Grant Cloud Run SA access to read secrets
resource "google_secret_manager_secret_iam_member" "secret_access_id" {
  secret_id = data.google_secret_manager_secret.opensky_client_id.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.cloud_run_sa.email}"
}

resource "google_secret_manager_secret_iam_member" "secret_access_secret" {
  secret_id = data.google_secret_manager_secret.opensky_client_secret.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.cloud_run_sa.email}"
}

resource "google_secret_manager_secret_iam_member" "secret_access_push_token" {
  secret_id = data.google_secret_manager_secret.ingest_push_token.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.cloud_run_sa.email}"
}

# Grant Cloud Run SA ability to PAUSE/RESUME scheduler jobs (Management API)
resource "google_project_iam_member" "scheduler_admin" {
  project = var.project_id
  role    = "roles/cloudscheduler.admin"
  member  = "serviceAccount:${google_service_account.cloud_run_sa.email}"
}

# Grant Cloud Run SA access to Firestore
resource "google_project_iam_member" "firestore_user" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.cloud_run_sa.email}"
}

# ---------------------------------------------------------------------------
# Firestore TTL: auto-expire old telemetry points to keep storage in the
# free tier. Documents are written with `expire_at = ingested_at + 24h`
# (safely beyond the 12h history read window); Firestore deletes them after
# that timestamp at no cost.
# ---------------------------------------------------------------------------
resource "google_firestore_field" "vehicles_ttl" {
  project    = var.project_id
  database   = "(default)"
  collection = "vehicles"
  field      = "expire_at"

  ttl_config {}
}

# ---------------------------------------------------------------------------
# Cloud Run Service (API + Workers)
# ---------------------------------------------------------------------------
resource "google_cloud_run_v2_service" "telemetry_api" {
  name     = "telemetry-api"
  location = var.region

  template {
    service_account = google_service_account.cloud_run_sa.email

    # Scale to zero when idle; cap the ceiling so a traffic spike or crash
    # loop can never run up a bill on a portfolio demo.
    scaling {
      min_instance_count = 0
      max_instance_count = 2
    }

    containers {
      image = var.image_url # CI/CD will provide the actual image URI

      # CPU is only allocated during request processing (default for v2 when
      # min_instance_count = 0), so there is no charge while idle.
      resources {
        cpu_idle = true
      }

      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }
      env {
        name  = "FIRESTORE_DB_ID"
        value = "(default)"
      }
      env {
        name  = "GCP_LOCATION"
        value = var.region
      }
      env {
        name  = "OPENSKY_BBOX"
        value = var.opensky_bbox
      }

      env {
        name = "OPENSKY_CLIENT_ID"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.opensky_client_id.secret_id
            version = "latest"
          }
        }
      }

      env {
        name = "OPENSKY_CLIENT_SECRET"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.opensky_client_secret.secret_id
            version = "latest"
          }
        }
      }

      env {
        name = "INGEST_PUSH_TOKEN"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.ingest_push_token.secret_id
            version = "latest"
          }
        }
      }
    }
  }
}

# The Cloud Run service is publicly accessible (allUsers) so the dashboard
# SPA and read-only API can be reached without authentication. Write endpoints
# (/ingest/*, /api/manage/start|stop) have their own auth guards (OIDC header,
# bearer tokens) enforced in the application code.
resource "google_cloud_run_service_iam_member" "public" {
  location = google_cloud_run_v2_service.telemetry_api.location
  project  = google_cloud_run_v2_service.telemetry_api.project
  service  = google_cloud_run_v2_service.telemetry_api.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# ---------------------------------------------------------------------------
# Cloud Scheduler (The Timer Trigger)
# ---------------------------------------------------------------------------
# Service Account for Scheduler to invoke Cloud Run securely via OIDC.
resource "google_service_account" "scheduler_sa" {
  account_id   = "telemetry-scheduler-sa"
  display_name = "Telemetry Platform Scheduler Invoker"
}

# Only the Metro feed is polled on-cloud. Flights are pushed from an off-cloud
# host, so there is no flight-ingester job here (it would just POST to a
# GCP-blocked OpenSky endpoint and burn Cloud Run invocations).
resource "google_cloud_scheduler_job" "metro_ingester" {
  name             = "metro-ingester"
  description      = "Polls Metro GTFS-RT feed"
  schedule         = var.metro_polling_cron
  time_zone        = "America/Chicago"
  attempt_deadline = "30s"

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.telemetry_api.uri}/ingest/metro"
    headers = {
      "X-CloudScheduler" = "true"
    }
    oidc_token {
      service_account_email = google_service_account.scheduler_sa.email
    }
  }
}
