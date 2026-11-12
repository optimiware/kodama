variable "account_id" {
  description = "AWS account ID that owns the OpenTofu state bucket."
  type        = string
}

variable "region" {
  description = "AWS region for the OpenTofu state bucket."
  type        = string
  default     = "eu-west-1"
}

variable "bucket_name" {
  description = "Globally unique S3 bucket name for OpenTofu state."
  type        = string
}

variable "noncurrent_version_expiration_days" {
  description = "Days to retain noncurrent state and .tflock versions."
  type        = number
  default     = 30
}

variable "force_destroy" {
  description = "Allow bucket deletion when non-empty (keep false for shared multi-cluster state buckets)."
  type        = bool
  default     = false
}
