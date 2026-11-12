variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "cluster_name" {
  description = "Logical Kodama cluster name (defaults to stage)."
  type        = string
  default     = "stage"
}

variable "environment" {
  description = "Kodama runtime environment profile (production for stage and prod clusters)."
  type        = string
  default     = "production"
}

variable "cost_profile" {
  description = "Infrastructure sizing profile (cost-conscious or ha)."
  type        = string
  default     = "cost-conscious"
}

variable "region" {
  description = "GCP region."
  type        = string
  default     = "europe-west1"
}

variable "zone" {
  description = "Primary GCP zone (defaults to <region>-b when empty)."
  type        = string
  default     = ""
}

variable "enable_apis" {
  description = "Enable required GCP service APIs via google_project_service."
  type        = bool
  default     = true
}

variable "release_namespace" {
  description = "Kubernetes namespace for the Kodama control-plane release."
  type        = string
  default     = "kodama-system"
}

variable "tenants" {
  description = "Declared tenant slugs for isolated tenant namespaces."
  type        = list(string)
  default     = ["tenant-a"]
}

variable "subnet_cidr" {
  description = "Primary CIDR range for the GKE subnetwork."
  type        = string
  default     = "10.40.0.0/20"
}

variable "pods_cidr" {
  description = "Secondary CIDR range for GKE pods."
  type        = string
  default     = "10.48.0.0/14"
}

variable "services_cidr" {
  description = "Secondary CIDR range for GKE services."
  type        = string
  default     = "10.52.0.0/20"
}

variable "gke_release_channel" {
  description = "GKE release channel."
  type        = string
  default     = "REGULAR"
}

variable "gke_machine_type" {
  description = "GCE machine type for the GKE worker pool."
  type        = string
  default     = "e2-standard-2"
}

variable "gke_node_count" {
  description = "Initial GKE worker node count."
  type        = number
  default     = 2
}

variable "gke_min_node_count" {
  description = "Minimum GKE worker node count."
  type        = number
  default     = 1
}

variable "gke_max_node_count" {
  description = "Maximum GKE worker node count."
  type        = number
  default     = 4
}

variable "gke_disk_size_gb" {
  description = "Worker boot persistent disk size in GiB."
  type        = number
  default     = 50
}

variable "gke_deletion_protection" {
  description = "Enable GKE cluster deletion protection."
  type        = bool
  default     = false
}

variable "k8s_api_allowed_cidrs" {
  description = "Non-empty list of operator networks (CIDR) allowed to reach the GKE control-plane endpoint. Open ranges (0.0.0.0/0, ::/0, or any /0 prefix) are refused."
  type        = list(string)
  nullable    = false

  validation {
    condition     = length(var.k8s_api_allowed_cidrs) > 0
    error_message = "k8s_api_allowed_cidrs must not be empty; supply at least one narrow operator CIDR such as 203.0.113.7/32."
  }

  validation {
    condition     = alltrue([for c in var.k8s_api_allowed_cidrs : can(cidrhost(c, 0))])
    error_message = "k8s_api_allowed_cidrs entries must be valid CIDR blocks such as 203.0.113.7/32."
  }

  validation {
    condition     = alltrue([for c in var.k8s_api_allowed_cidrs : can(regex("/[1-9][0-9]*$", c))])
    error_message = "k8s_api_allowed_cidrs must not contain 0.0.0.0/0, ::/0 or any effectively open /0 range; use a narrow prefix such as 203.0.113.7/32."
  }
}

variable "workspace_storage_class" {
  description = "Kubernetes StorageClass used by Kodama for execution workspace PVCs and retained logs."
  type        = string
  default     = "standard-rwo"
}

variable "postgres_version" {
  description = "Cloud SQL PostgreSQL version."
  type        = string
  default     = "POSTGRES_16"
}

variable "postgres_tier" {
  description = "Cloud SQL machine tier."
  type        = string
  default     = "db-custom-1-3840"
}

variable "postgres_disk_size_gb" {
  description = "Cloud SQL SSD storage size in GiB."
  type        = number
  default     = 20
}

variable "postgres_admin_user" {
  description = "Bootstrap administrator username for Cloud SQL PostgreSQL."
  type        = string
  default     = "kodama_bootstrap_admin"
}

variable "postgres_admin_password" {
  description = "Bootstrap administrator password for Cloud SQL PostgreSQL (passed via TF_VAR_postgres_admin_password and written with password_wo)."
  type        = string
  sensitive   = true
}

variable "postgres_password_wo_version" {
  description = "Write-only password version counter for google_sql_user."
  type        = number
  default     = 1
}

variable "postgres_backup_retention_count" {
  description = "Number of retained automated backups (S10 requires 30-day recovery)."
  type        = number
  default     = 30
}

variable "postgres_deletion_protection" {
  description = "Enable Cloud SQL deletion protection."
  type        = bool
  default     = false
}

variable "kms_rotation_period" {
  description = "Automatic rotation period for the Cloud KMS symmetric key."
  type        = string
  default     = "2592000s"
}

variable "extra_tags" {
  description = "Additional labels applied to GCP resources."
  type        = map(string)
  default     = {}
}
