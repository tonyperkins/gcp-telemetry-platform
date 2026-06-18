output "service_url" {
  value       = google_cloud_run_v2_service.telemetry_api.uri
  description = "The URL of the Telemetry API Cloud Run service"
}

output "project_id" {
  value = var.project_id
}

output "region" {
  value = var.region
}
