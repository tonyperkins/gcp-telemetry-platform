terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
  }
  # Note: Add remote backend (GCS) before applying to production
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# Enable Required APIs
resource "google_project_service" "compute" {
  service            = "compute.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "vpcaccess" {
  service            = "vpcaccess.googleapis.com"
  disable_on_destroy = false
}

# ---------------------------------------------------------------------------
# Networking: VPC + Cloud NAT (for Static Egress IP)
# ---------------------------------------------------------------------------

resource "google_compute_network" "telemetry_vpc" {
  name                    = "telemetry-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "telemetry_subnet" {
  name          = "telemetry-subnet"
  ip_cidr_range = "10.10.0.0/24"
  region        = var.region
  network       = google_compute_network.telemetry_vpc.id
}

# Serverless VPC Access Connector
resource "google_vpc_access_connector" "connector" {
  name          = "telemetry-connector"
  region        = var.region
  ip_cidr_range = "10.124.0.0/28"
  network       = google_compute_network.telemetry_vpc.name
  
  # Min/Max instances for cost/scaling
  min_instances = 2
  max_instances = 3

  depends_on = [google_project_service.vpcaccess]
}

# Static IP for the NAT
resource "google_compute_address" "nat_ip" {
  name   = "telemetry-nat-ip"
  region = var.region
}

# Router for NAT
resource "google_compute_router" "router" {
  name    = "telemetry-router"
  region  = var.region
  network = google_compute_network.telemetry_vpc.id
}

# NAT Gateway
resource "google_compute_router_nat" "nat" {
  name                               = "telemetry-nat"
  router                             = google_compute_router.router.name
  region                             = var.region
  nat_ip_allocate_option             = "MANUAL_ONLY"
  nat_ips                            = [google_compute_address.nat_ip.self_link]
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# ---------------------------------------------------------------------------
# Secret Manager for OpenSky Credentials
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
# Cloud Run Service (API + Workers)
# ---------------------------------------------------------------------------
resource "google_cloud_run_v2_service" "telemetry_api" {
  name     = "telemetry-api"
  location = var.region

  template {
    service_account = google_service_account.cloud_run_sa.email
    containers {
      image = var.image_url # CI/CD will provide the actual image URI

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
        name = "OPENSKY_BBOX"
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

    vpc_access {
      connector = google_vpc_access_connector.connector.id
      egress    = "ALL_TRAFFIC"
    }
  }
}

# Make the Cloud Run service publicly accessible
resource "google_cloud_run_service_iam_member" "public" {
  location = google_cloud_run_v2_service.telemetry_api.location
  project  = google_cloud_run_v2_service.telemetry_api.project
  service  = google_cloud_run_v2_service.telemetry_api.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# ---------------------------------------------------------------------------
# Cloud Scheduler (The Timer Triggers)
# ---------------------------------------------------------------------------
# Service Account for Scheduler to invoke Cloud Run securely
resource "google_service_account" "scheduler_sa" {
  account_id   = "telemetry-scheduler-sa"
  display_name = "Telemetry Platform Scheduler Invoker"
}

# Scheduler needs invoker role, but our Cloud Run is public anyway for the API.
# However, standard practice is to use OIDC tokens for the /ingest endpoints.

resource "google_cloud_scheduler_job" "metro_ingester" {
  name             = "metro-ingester"
  description      = "Polls Metro GTFS-RT feed"
  schedule         = "*/1 * * * *" # Every 1 minute
  time_zone        = "America/Chicago"
  attempt_deadline = "30s"

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.telemetry_api.uri}/ingest/metro"
    headers     = {
      "X-CloudScheduler" = "true"
    }
    oidc_token {
      service_account_email = google_service_account.scheduler_sa.email
    }
  }
}

resource "google_cloud_scheduler_job" "flight_ingester" {
  name             = "flight-ingester"
  description      = "Polls OpenSky API"
  schedule         = var.flight_polling_cron
  time_zone        = "America/Chicago"
  attempt_deadline = "30s"

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.telemetry_api.uri}/ingest/flight"
    headers     = {
      "X-CloudScheduler" = "true"
    }
    oidc_token {
      service_account_email = google_service_account.scheduler_sa.email
    }
  }
}
