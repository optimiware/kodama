output "bucket_name" {
  description = "Provisioned AWS S3 bucket for OpenTofu state."
  value       = aws_s3_bucket.tofu_state.bucket
}

output "region" {
  description = "AWS region of the state bucket."
  value       = var.region
}

output "account_id" {
  description = "AWS account ID owning the state bucket."
  value       = var.account_id
}

output "state_key_template" {
  description = "Per-cluster S3 state key template."
  value       = "kodama/aws/${var.account_id}/${var.region}/<cluster_name>/infrastructure.tfstate"
}
