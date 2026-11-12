output "bucket_name" {
  description = "Provisioned GCS bucket for OpenTofu state."
  value       = google_storage_bucket.tofu_state.name
}

output "region" {
  description = "GCP region of the state bucket."
  value       = var.region
}

output "project_id" {
  description = "GCP project ID owning the state bucket."
  value       = var.project_id
}

output "state_prefix_template" {
  description = "Per-cluster GCS state prefix template."
  value       = "kodama/gcp/${var.project_id}/${var.region}/<cluster_name>"
}
