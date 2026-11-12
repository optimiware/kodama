output "bucket_name" {
  description = "Provisioned Scaleway Object Storage bucket for OpenTofu state."
  value       = scaleway_object_bucket.tofu_state.name
}

output "region" {
  description = "Scaleway region of the state bucket."
  value       = var.region
}

output "project_id" {
  description = "Scaleway project ID owning the state bucket."
  value       = var.project_id
}

output "s3_endpoint" {
  description = "Scaleway S3-compatible endpoint URL."
  value       = "https://s3.${var.region}.scw.cloud"
}

output "state_key_template" {
  description = "Per-cluster state key template inside the bucket."
  value       = "kodama/scw/${var.project_id}/${var.region}/<cluster_name>/infrastructure.tfstate"
}
