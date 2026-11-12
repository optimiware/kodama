variable "account_id" {
  description = "AWS 12-digit account ID."
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
  description = "AWS region."
  type        = string
  default     = "eu-west-1"
}

variable "availability_zones" {
  description = "Two availability zones in the selected AWS region (defaults to [<region>a, <region>b] when empty)."
  type        = list(string)
  default     = []
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

variable "vpc_cidr" {
  description = "VPC IPv4 CIDR block."
  type        = string
  default     = "10.60.0.0/16"
}

variable "public_subnet_cidrs" {
  description = "Public subnet CIDRs across the two availability zones."
  type        = list(string)
  default     = ["10.60.1.0/24", "10.60.2.0/24"]
}

variable "private_subnet_cidrs" {
  description = "Private subnet CIDRs across the two availability zones."
  type        = list(string)
  default     = ["10.60.11.0/24", "10.60.12.0/24"]
}

variable "eks_version" {
  description = "Amazon EKS Kubernetes version."
  type        = string
  default     = "1.35"
}

variable "eks_instance_types" {
  description = "EC2 instance types for the EKS managed node group."
  type        = list(string)
  default     = ["t3.medium"]
}

variable "eks_desired_size" {
  description = "Desired worker node count."
  type        = number
  default     = 2
}

variable "eks_min_size" {
  description = "Minimum worker node count."
  type        = number
  default     = 1
}

variable "eks_max_size" {
  description = "Maximum worker node count."
  type        = number
  default     = 4
}

variable "eks_disk_size_gb" {
  description = "Worker node EBS root volume size in GiB."
  type        = number
  default     = 40
}

variable "k8s_api_allowed_cidrs" {
  description = "Non-empty list of operator networks (CIDR) allowed to reach the public EKS Kubernetes API endpoint. Open ranges (0.0.0.0/0, ::/0, or any /0 prefix) are refused."
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
  default     = "gp3"
}

variable "postgres_engine_version" {
  description = "Amazon RDS PostgreSQL major version (or major.minor)."
  type        = string
  default     = "16"

  validation {
    condition     = can(regex("^[1-9][0-9]+(\\.[0-9]+)?$", var.postgres_engine_version))
    error_message = "postgres_engine_version must be a numeric PostgreSQL major or major.minor version such as 16 or 17.2."
  }
}

variable "postgres_ca_cert_identifier" {
  description = "Amazon RDS CA certificate identifier wired to aws_db_instance.ca_cert_identifier (for example rds-ca-rsa2048-gss or rds-ca-ecc384-g1). This identifier selects the server certificate; it is not a PEM bundle and must match the operator-supplied database_ca_bundle."
  type        = string
  default     = "rds-ca-rsa2048-gss"

  validation {
    condition     = can(regex("^rds-ca-[a-z0-9][a-z0-9-]*$", var.postgres_ca_cert_identifier))
    error_message = "postgres_ca_cert_identifier must be an Amazon RDS CA identifier such as rds-ca-rsa2048-gss; a PEM bundle or an empty placeholder is refused."
  }
}

variable "database_ca_bundle" {
  description = "Operator-supplied public Amazon RDS CA certificate PEM bundle for this region, exported as database_ca_certificate and mounted as ca.crt by scripts/bootstrap-secrets. Supply the public bundle from the AWS RDS trust store (for example the global bundle); never a private key, an RDS CA identifier or a cluster CA. Rotate it together with postgres_ca_cert_identifier."
  type        = string
  sensitive   = true
  nullable    = false

  validation {
    condition = (
      length(trimspace(var.database_ca_bundle)) > 0 &&
      !strcontains(var.database_ca_bundle, "PRIVATE KEY") &&
      can(regex("(?s)^\\s*(?:-----BEGIN CERTIFICATE-----\\s*[A-Za-z0-9+/=][A-Za-z0-9+/=\\s]*-----END CERTIFICATE-----\\s*)+$", var.database_ca_bundle))
    )
    error_message = "database_ca_bundle must be a non-empty public RDS CA PEM bundle: certificate BEGIN/END markers must form complete ordered blocks whose bodies carry no further markers (no reversed, duplicate, extra or missing markers) and a PRIVATE KEY block is refused. An empty placeholder, an identifier or private key material is refused."
  }
}

variable "postgres_instance_class" {
  description = "Amazon RDS PostgreSQL instance class."
  type        = string
  default     = "db.t4g.small"
}

variable "postgres_allocated_storage_gb" {
  description = "Amazon RDS gp3 allocated storage in GiB."
  type        = number
  default     = 20
}

variable "postgres_admin_user" {
  description = "Bootstrap administrator username for Amazon RDS PostgreSQL."
  type        = string
  default     = "kodama_bootstrap_admin"
}

variable "postgres_admin_password" {
  description = "Bootstrap administrator password for Amazon RDS PostgreSQL (passed via TF_VAR_postgres_admin_password and written with password_wo)."
  type        = string
  sensitive   = true
}

variable "postgres_password_wo_version" {
  description = "Write-only password version counter for aws_db_instance."
  type        = number
  default     = 1
}

variable "postgres_backup_retention_days" {
  description = "Automated RDS backup retention period in days (S10 requires 30-day recovery)."
  type        = number
  default     = 30
}

variable "postgres_skip_final_snapshot" {
  description = "Skip final RDS snapshot on cluster teardown."
  type        = bool
  default     = true
}

variable "kms_deletion_window_days" {
  description = "Waiting period in days before AWS KMS deletes the key on teardown."
  type        = number
  default     = 7
}

variable "extra_tags" {
  description = "Additional tags applied to AWS resources."
  type        = map(string)
  default     = {}
}
