variable "project_id" {
  description = "Scaleway project ID."
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
  description = "Scaleway region."
  type        = string
  default     = "fr-par"
}

variable "zone" {
  description = "Scaleway availability zone (defaults to <region>-1; Managed PostgreSQL private networks require the region's default AZ)."
  type        = string
  default     = ""
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

variable "kubernetes_version" {
  description = "Scaleway Kapsule Kubernetes minor version (or minor.patch)."
  type        = string
  default     = "1.37"
}

variable "k8s_node_type" {
  description = "Scaleway instance type for the Kapsule worker pool."
  type        = string
  default     = "PLAY2-NANO"
}

variable "k8s_pool_size" {
  description = "Initial worker node count."
  type        = number
  default     = 2
}

variable "k8s_api_allowed_cidrs" {
  description = "Non-empty list of operator networks (IPv4 CIDR) allowed to reach the Kubernetes API, besides Scaleway's own ranges. Set per environment, for example through TF_VAR_k8s_api_allowed_cidrs='[\"203.0.113.7/32\"]'."
  type        = list(string)
  nullable    = false

  validation {
    condition     = length(var.k8s_api_allowed_cidrs) > 0
    error_message = "k8s_api_allowed_cidrs must not be empty; supply at least one narrow operator IPv4 CIDR such as 203.0.113.7/32."
  }

  validation {
    condition     = alltrue([for c in var.k8s_api_allowed_cidrs : can(cidrnetmask(c))])
    error_message = "k8s_api_allowed_cidrs entries must be IPv4 CIDR blocks such as 203.0.113.7/32."
  }

  # cidrnetmask above accepts every /0 spelling (0.0.0.0/0, 0.0.0.1/0,
  # 10.0.0.0/0, 0.0.0.0/00), so an exact-string deny list is not enough: a
  # /0 prefix permits the whole address space whatever the host bits say. A
  # non-zero prefix length refuses every effectively open range and accepts a
  # narrow /1../32. The IPv6 open range is refused by the IPv4 clause above.
  validation {
    condition     = alltrue([for c in var.k8s_api_allowed_cidrs : can(regex("/([1-9]|[12][0-9]|3[0-2])$", c))])
    error_message = "k8s_api_allowed_cidrs must not contain 0.0.0.0/0, ::/0 or any effectively open /0 range; use a narrow prefix such as 203.0.113.7/32."
  }
}

variable "k8s_pool_min_size" {
  description = "Minimum worker node count when autoscaling is enabled."
  type        = number
  default     = 1
}

variable "k8s_pool_max_size" {
  description = "Maximum worker node count when autoscaling is enabled."
  type        = number
  default     = 4
}

variable "k8s_autoscaling" {
  description = "Enable Kapsule node pool autoscaling."
  type        = bool
  default     = true
}

variable "k8s_root_volume_size_gb" {
  description = "Worker node root SBS volume size in GiB."
  type        = number
  default     = 40
}

variable "workspace_storage_class" {
  description = "Kubernetes StorageClass used by Kodama for execution workspace PVCs and retained logs."
  type        = string
  default     = "sbs-default"
}

variable "postgres_engine" {
  description = "Scaleway Managed Database PostgreSQL engine version."
  type        = string
  default     = "PostgreSQL-17"
}

variable "postgres_node_type" {
  description = "Scaleway Managed Database node type (must support SBS block storage when volume_type is sbs_5k)."
  type        = string
  default     = "db-play2-pico"
}

variable "postgres_volume_size_gb" {
  description = "Scaleway Managed Database SBS block storage volume size in GiB."
  type        = number
  default     = 20
}

variable "postgres_database_name" {
  description = "Application database name, created and owned by the migration step."
  type        = string
  default     = "kodama"
}

variable "postgres_admin_database" {
  description = "Maintenance database the administrator connects to in order to create the application database (the instance's default database)."
  type        = string
  default     = "rdb"
}

variable "postgres_admin_user" {
  description = "Initial bootstrap administrator username for the Managed PostgreSQL instance."
  type        = string
  default     = "kodama_bootstrap_admin"
}

variable "postgres_admin_password" {
  description = "Initial bootstrap administrator password for the Managed PostgreSQL instance (passed via TF_VAR_postgres_admin_password and written with password_wo so it is not persisted in state)."
  type        = string
  sensitive   = true
}

variable "postgres_password_wo_version" {
  description = "Write-only password version counter for scaleway_rdb_instance."
  type        = number
  default     = 1
}

variable "postgres_backup_frequency_hours" {
  description = "Automated logical backup interval in hours."
  type        = number
  default     = 6
}

variable "postgres_backup_retention_days" {
  description = "Automated backup retention window in days (S10 requires 30-day recovery)."
  type        = number
  default     = 30
}

variable "kms_rotation_period" {
  description = "Automatic rotation period for the Scaleway Key Manager key."
  type        = string
  default     = "720h"
}

variable "kms_unprotected" {
  description = "Allow deletion of the KMS key on cluster teardown (defaults to false so backups remain decryptable)."
  type        = bool
  default     = false
}

variable "extra_tags" {
  description = "Additional tags applied to Scaleway resources."
  type        = map(string)
  default     = {}
}
