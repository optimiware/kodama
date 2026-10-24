variable "project_id" {
  description = "Google Cloud project ID hosting the dedicated stage cluster and services."
  type        = string
}

variable "installation_id" {
  description = "Stable Kodama installation identifier (DNS label; binds envelope encryption)."
  type        = string
  default     = "kodama-stage-gcp"
  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.installation_id)) && length(var.installation_id) <= 63
    error_message = "installation_id must be a valid DNS label (<= 63 chars)."
  }
}

variable "region" {
  description = "GCP region for GKE, Cloud SQL, GCS and Cloud KMS."
  type        = string
  default     = "europe-west1"
}

variable "environment_tier" {
  description = "Target tier: staging uses ENTERPRISE with 7-day PITR; production uses ENTERPRISE_PLUS with >= 30-day PITR."
  type        = string
  default     = "staging"
  validation {
    condition     = contains(["staging", "production"], var.environment_tier)
    error_message = "environment_tier must be staging or production."
  }
}

variable "subnet_cidr" {
  description = "Primary CIDR for the GKE subnet."
  type        = string
  default     = "10.43.0.0/20"
}

variable "pods_cidr" {
  description = "Secondary CIDR for GKE Pods."
  type        = string
  default     = "10.44.0.0/16"
}

variable "services_cidr" {
  description = "Secondary CIDR for GKE Services."
  type        = string
  default     = "10.45.0.0/20"
}

variable "node_machine_type" {
  description = "GCE machine type for GKE worker nodes."
  type        = string
  default     = "e2-standard-4"
}

variable "node_count" {
  description = "Worker nodes per zone."
  type        = number
  default     = 1
}

variable "db_tier" {
  description = "Cloud SQL machine tier (staging default db-custom-2-7680; production uses db-perf-optimized-N-4+)."
  type        = string
  default     = "db-custom-2-7680"
}

variable "db_pitr_retention_days" {
  description = "Cloud SQL PITR transaction log retention days (1-7 on ENTERPRISE, up to 35 on ENTERPRISE_PLUS)."
  type        = number
  default     = 7
  validation {
    condition     = var.db_pitr_retention_days >= 1 && var.db_pitr_retention_days <= 35
    error_message = "db_pitr_retention_days must be between 1 and 35."
  }
}

variable "ingress_host" {
  description = "HTTPS hostname serving the Kodama public and runtime callback API."
  type        = string
  default     = "kodama.stage.gcp.example.invalid"
}

variable "runtime_callback_cidrs" {
  description = "Explicit CIDRs tenant workload Pods may reach on 443 for the Kodama runtime callback."
  type        = list(string)
  default     = ["10.43.0.0/20", "10.45.0.0/20"]
}

variable "dependency_egress_rules" {
  description = "Explicit CIDR egress rules (Git HTTPS, GCS XML API, OCI registry) allowed from tenant execution namespaces."
  type = list(object({
    name  = string
    cidr  = string
    ports = list(number)
  }))
  default = [
    {
      name  = "git-https"
      cidr  = "198.51.100.0/24"
      ports = [443]
    },
    {
      name  = "object-storage"
      cidr  = "203.0.113.0/24"
      ports = [443]
    },
    {
      name  = "oci-registry"
      cidr  = "192.0.2.0/24"
      ports = [443]
    }
  ]
}

variable "tenants" {
  description = "Tenant execution namespaces and tenant IDs."
  type = list(object({
    namespace = string
    tenant_id = string
  }))
  default = [
    {
      namespace = "kodama-tenant-stage"
      tenant_id = "t_stage"
    }
  ]
}
