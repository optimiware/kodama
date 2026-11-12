variable "cloud" {
  description = "Cloud target identifier (scw, gcp, aws)."
  type        = string

  validation {
    condition     = contains(["scw", "gcp", "aws"], var.cloud)
    error_message = "cloud must be one of: scw, gcp, aws."
  }
}

variable "cluster_name" {
  description = "Logical Kodama cluster name within the target cloud/project/region (defaults to stage)."
  type        = string
  default     = "stage"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,16}[a-z0-9]$", var.cluster_name))
    error_message = "cluster_name must be 2-18 lowercase alphanumeric characters or hyphens, starting with a letter and ending with an alphanumeric character (so derived GCP service account IDs stay within 30 chars)."
  }
}

variable "environment" {
  description = "Kodama installation environment profile (production, test, development). Stage and prod clusters run the production profile."
  type        = string
  default     = "production"

  validation {
    condition     = contains(["production", "test", "development"], var.environment)
    error_message = "environment must be one of: production, test, development."
  }
}

variable "cost_profile" {
  description = "Sizing profile for infrastructure resources (cost-conscious or ha)."
  type        = string
  default     = "cost-conscious"

  validation {
    condition     = contains(["cost-conscious", "ha"], var.cost_profile)
    error_message = "cost_profile must be either cost-conscious or ha."
  }
}

variable "account_or_project_id" {
  description = "Cloud account ID (AWS) or project ID (Scaleway, GCP) owning the installation."
  type        = string

  validation {
    condition     = length(trimspace(var.account_or_project_id)) > 0
    error_message = "account_or_project_id must not be empty."
  }
}

variable "region" {
  description = "Primary cloud region."
  type        = string

  validation {
    condition     = length(trimspace(var.region)) > 0
    error_message = "region must not be empty."
  }
}

variable "zone" {
  description = "Primary cloud availability zone."
  type        = string
  default     = ""
}

variable "release_namespace" {
  description = "Kubernetes namespace for the Kodama control-plane release."
  type        = string
  default     = "kodama-system"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.release_namespace))
    error_message = "release_namespace must be a valid DNS-1123 label."
  }
}

variable "tenants" {
  description = "Declared tenant slugs to provision isolated Kubernetes namespaces and policies for."
  type        = list(string)
  default     = ["tenant-a"]

  validation {
    condition     = length(var.tenants) > 0 && alltrue([for t in var.tenants : can(regex("^[a-z0-9]([-a-z0-9]{0,47}[a-z0-9])?$", t))])
    error_message = "tenants must be a non-empty list of valid DNS-1123 tenant slugs of at most 49 characters (so kodama-tenant-<id> stays within 63 characters)."
  }
}

variable "extra_tags" {
  description = "Additional key-value tags/labels applied to cloud resources."
  type        = map(string)
  default     = {}
}
