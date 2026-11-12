variable "project_id" {
  description = "Scaleway project ID that owns the OpenTofu state bucket."
  type        = string
}

variable "region" {
  description = "Scaleway region for the OpenTofu state bucket."
  type        = string
  default     = "fr-par"
}

variable "bucket_name" {
  description = "Globally unique Scaleway Object Storage bucket name for OpenTofu state."
  type        = string
}

variable "noncurrent_version_expiration_days" {
  description = "Days to retain noncurrent state and lockfile object versions."
  type        = number
  default     = 30
}

variable "force_destroy" {
  description = "Allow bucket deletion when non-empty (keep false for shared multi-cluster state buckets)."
  type        = bool
  default     = false
}
