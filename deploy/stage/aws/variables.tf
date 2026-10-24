variable "installation_id" {
  description = "Stable Kodama installation identifier (DNS label; binds envelope encryption)."
  type        = string
  default     = "kodama-stage-aws"
  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.installation_id)) && length(var.installation_id) <= 63
    error_message = "installation_id must be a valid DNS label (<= 63 chars)."
  }
}

variable "region" {
  description = "AWS region for the EKS cluster, RDS PostgreSQL, S3 buckets and KMS keys."
  type        = string
  default     = "eu-west-1"
}

variable "environment_tier" {
  description = "Target tier: staging uses lower-cost sizing and reduced retention; production enforces 30-day PITR."
  type        = string
  default     = "staging"
  validation {
    condition     = contains(["staging", "production"], var.environment_tier)
    error_message = "environment_tier must be staging or production."
  }
}

variable "vpc_cidr" {
  description = "CIDR block for the dedicated stage VPC."
  type        = string
  default     = "10.42.0.0/16"
}

variable "kubernetes_version" {
  description = "Pinned Amazon EKS Kubernetes control-plane version."
  type        = string
  default     = "1.35"
}

variable "node_instance_type" {
  description = "Worker node instance type for the trusted-engineer baseline."
  type        = string
  default     = "m6i.large"
}

variable "node_count" {
  description = "Desired worker node count."
  type        = number
  default     = 2
}

variable "db_instance_class" {
  description = "RDS PostgreSQL instance class (staging default is burstable; production uses db.m6i.large+)."
  type        = string
  default     = "db.t4g.medium"
}

variable "db_engine_version" {
  description = "RDS PostgreSQL engine version (fresh-install default 17.11; pin in tfvars for existing installations)."
  type        = string
  default     = "17.11"
}

variable "db_allow_major_version_upgrade" {
  description = "Explicit opt-in for RDS PostgreSQL major version upgrades after a verified snapshot/PITR checkpoint."
  type        = bool
  default     = false
}

variable "db_allocated_storage_gb" {
  description = "Initial RDS storage in GiB."
  type        = number
  default     = 50
}

variable "db_backup_retention_days" {
  description = "Automated RDS backup/PITR window in days (staging default 7; production gate requires >= 30)."
  type        = number
  default     = 7
  validation {
    condition     = var.db_backup_retention_days >= 1 && var.db_backup_retention_days <= 35
    error_message = "db_backup_retention_days must be between 1 and 35."
  }
}

variable "ingress_host" {
  description = "HTTPS hostname serving the Kodama public and runtime callback API."
  type        = string
  default     = "kodama.stage.aws.example.invalid"
}

variable "runtime_callback_cidrs" {
  description = "Explicit CIDRs tenant workload Pods may reach on 443 for the Kodama runtime callback."
  type        = list(string)
  default     = ["10.42.0.0/16"]
}

variable "dependency_egress_rules" {
  description = "Explicit CIDR egress rules (Git HTTPS, S3, OCI registry) allowed from tenant execution namespaces."
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
