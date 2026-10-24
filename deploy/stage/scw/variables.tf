variable "project_id" {
  description = "Scaleway project UUID hosting the stage cluster, RDB, Object Storage and Key Manager."
  type        = string
}

variable "installation_id" {
  description = "Stable Kodama installation identifier (DNS label; binds envelope encryption)."
  type        = string
  default     = "kodama-stage-scw"
  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.installation_id)) && length(var.installation_id) <= 63
    error_message = "installation_id must be a valid DNS label (<= 63 chars)."
  }
}

variable "region" {
  description = "Scaleway region. Only fr-par is supported by Kodama's Scaleway Key Manager adapter."
  type        = string
  default     = "fr-par"
  validation {
    condition     = var.region == "fr-par"
    error_message = "Kodama's Scaleway Key Manager adapter only supports region fr-par."
  }
}

variable "environment_tier" {
  description = "Target tier: staging uses lower-cost sizing and records scheduled backup limits; production requires a qualifying PITR profile."
  type        = string
  default     = "staging"
  validation {
    condition     = contains(["staging", "production"], var.environment_tier)
    error_message = "environment_tier must be staging or production."
  }
}

variable "private_network_cidr" {
  description = "IPv4 CIDR block for the Scaleway VPC Private Network."
  type        = string
  default     = "172.16.40.0/22"
}

variable "kubernetes_version" {
  description = "Pinned Scaleway Kapsule Kubernetes patch version (x.y.z; Kapsule rejects a bare minor without auto-upgrade)."
  type        = string
  default     = "1.35.9"
}

variable "node_type" {
  description = "Scaleway Kapsule worker node type."
  type        = string
  default     = "DEV1-L"
}

variable "node_count" {
  description = "Worker node count."
  type        = number
  default     = 2
}

variable "db_node_type" {
  description = "Scaleway Managed PostgreSQL node type (staging default DB-DEV-S; larger tiers use DB-GP-*)."
  type        = string
  default     = "DB-DEV-S"
}

variable "db_engine_version" {
  description = "Scaleway RDB PostgreSQL engine version (fresh-install default PostgreSQL-17; pin in tfvars for existing installations)."
  type        = string
  default     = "PostgreSQL-17"
}

variable "db_backup_frequency_hours" {
  description = "Scaleway RDB automated backup frequency in hours (staging default 24h; minimum 1h)."
  type        = number
  default     = 24
}

variable "db_backup_retention_days" {
  description = "Scaleway RDB automated backup retention in days."
  type        = number
  default     = 7
}

variable "ingress_host" {
  description = "HTTPS hostname serving the Kodama public and runtime callback API."
  type        = string
  default     = "kodama.stage.scw.example.invalid"
}

variable "runtime_callback_cidrs" {
  description = "Explicit CIDRs tenant workload Pods may reach on 443 for the Kodama runtime callback."
  type        = list(string)
  default     = ["172.16.40.0/22"]
}

variable "dependency_egress_rules" {
  description = "Explicit CIDR egress rules (Git HTTPS, Scaleway S3, OCI registry) allowed from tenant execution namespaces."
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

variable "operator_user_ids" {
  description = "Scaleway IAM user UUIDs that keep access to the buckets alongside the application identities. The OpenTofu operator must be listed or the bucket policy locks it out of refresh, update and destroy."
  type        = list(string)
  default     = []
}
