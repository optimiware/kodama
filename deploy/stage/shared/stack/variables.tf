variable "kubeconfig_path" {
  description = "Path to the kubeconfig file for the target stage cluster."
  type        = string
}

variable "stage_contract" {
  description = "Normalized stage_contract object emitted by deploy/stage/{aws,gcp,scw}."
  type = object({
    cloud            = string
    installation_id  = string
    region           = string
    environment_tier = string
    cluster = object({
      name           = string
      endpoint       = string
      ca_certificate = string
      oidc_issuer    = string
    })
    postgres = object({
      host                  = string
      port                  = number
      database              = string
      migration_role        = string
      runtime_role          = string
      ssl_mode              = string
      pitr_enabled          = bool
      backup_retention_days = number
      rpo_minutes_estimate  = number
      qualifies_o11_pitr    = bool
    })
    kms = object({
      provider               = string
      kek_id                 = string
      current                = string
      resources              = map(string)
      aws_region             = string
      scw_credentials_secret = string
    })
    object_storage = object({
      endpoint         = string
      region           = string
      artifacts_bucket = string
      artifacts_prefix = string
      logs_bucket      = string
      logs_prefix      = string
      path_style       = bool
    })
    identities = object({
      kodama_server_sa_annotations = map(string)
      loki_sa_annotations          = map(string)
    })
    network = object({
      runtime_url          = string
      ingress_host         = string
      runtime_egress_cidrs = list(string)
      dependency_egress_rules = list(object({
        name  = string
        cidr  = string
        ports = list(number)
      }))
    })
    tenants = list(object({
      namespace = string
      tenant_id = string
    }))
  })

  validation {
    condition     = contains(["aws", "gcp", "scw"], var.stage_contract.cloud) && var.stage_contract.cloud == var.stage_contract.kms.provider
    error_message = "stage_contract.cloud must match stage_contract.kms.provider (aws, gcp, or scw; no cross-provider fallback)."
  }
}

variable "bootstrap_secrets" {
  description = "Sensitive cloud material from the cloud root's stage_bootstrap_secrets output (database admin, object-store and KMS credentials)."
  type        = map(string)
  sensitive   = true
}

variable "control_plane_namespace" {
  description = "Kubernetes namespace for Kodama control plane and NATS JetStream."
  type        = string
  default     = "kodama-system"
}

variable "observability_namespace" {
  description = "Kubernetes namespace for Loki, Alloy, Prometheus and Grafana."
  type        = string
  default     = "kodama-observability"
}

variable "deps_namespace" {
  description = "Kubernetes namespace for the in-cluster Git server and OIDC issuer used by standalone stage installations."
  type        = string
  default     = "kodama-deps"
}

variable "acme_email" {
  description = "Contact email of the Let's Encrypt (ACME) account that issues the public ingress certificates."
  type        = string
  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", var.acme_email))
    error_message = "acme_email must be an email address."
  }
}

variable "acme_server" {
  description = "ACME directory URL (Let's Encrypt production by default; use the staging directory only for dry runs, its certificates are not trusted)."
  type        = string
  default     = "https://acme-v02.api.letsencrypt.org/directory"
}

variable "nats_tls" {
  description = "Whether NATS serves TLS. Stage default is false: plaintext nats:// with token authentication on the cluster network, which the chart accepts only under profile=development. Re-enabling TLS (profile=external) needs the private-CA design tracked by T-106."
  type        = bool
  default     = false
}

variable "issuer_allowed_cidrs" {
  description = "Operator networks (CIDRs) allowed to reach the OIDC issuer's token-issuing paths (/oauth2, /userinfo, /logout). Required: the issuer is a persona fixture and must never be reachable from everywhere."
  type        = list(string)
  validation {
    condition     = length(var.issuer_allowed_cidrs) > 0 && alltrue([for c in var.issuer_allowed_cidrs : can(cidrnetmask(c))])
    error_message = "issuer_allowed_cidrs needs at least one valid IPv4 CIDR."
  }
}

variable "oauthsonas_image" {
  description = "Digest-pinned oauthsonas image (the persona OIDC issuer of the stage authority)."
  type        = string
  default     = "ghcr.io/optimiware/oauthsonas:1.4.0@sha256:450a042217e49ac568a7b200d9636443e473c0e18e326231f18d2d88bc466c33"
}

variable "psql_image" {
  description = "Image of the short-lived Job that creates the database roles."
  type        = string
  default     = "docker.io/library/postgres:17-alpine@sha256:eee6c6e1b0894fa4fe0075eb054af9fd1cf39a35321c2d4110626cfe506411fb"
}

variable "kodama_image" {
  description = "Digest-pinned Kodama release image."
  type = object({
    repository = string
    tag        = string
    digest     = string
  })
  default = {
    repository = "ghcr.io/optimiware/kodama"
    tag        = "0.0.1"
    digest     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  }
}

variable "example_workload_image" {
  description = "Digest-pinned workload example image for the chart fixture."
  type = object({
    repository = string
    tag        = string
    digest     = string
  })
  default = {
    repository = "ghcr.io/optimiware/kodama-workload"
    tag        = "0.0.1"
    digest     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
  }
}

variable "git_url" {
  description = "HTTPS Git repository endpoint (defaults to https://git.<ingress_host>/optimi/kodama-source.git)."
  type        = string
  default     = ""
}

variable "registry_url" {
  description = "HTTPS OCI registry endpoint (defaults to https://registry.<ingress_host>)."
  type        = string
  default     = ""
}

variable "oidc_issuer" {
  description = "HTTPS OIDC issuer URL (defaults to https://auth.<ingress_host>)."
  type        = string
  default     = ""
}

variable "oidc_audience" {
  description = "Expected OIDC access-token audience for Kodama API (defaults to https://<ingress_host>/api)."
  type        = string
  default     = ""
}

variable "custom_ca" {
  description = "Whether the runtime and migration Secrets carry ca.crt for a private CA. Stage default false: public ingress certificates come from Let's Encrypt and the private-CA design is T-106."
  type        = bool
  default     = false
}

variable "storage_class" {
  description = "StorageClass for the kernel server data volume and per-run workspaces. The installation must qualify it for its CSI and ReadWriteOnce semantics."
  type        = string
  default     = "scw-bssd"
}

variable "chart_versions" {
  description = "Pinned upstream Helm chart versions for shared backend and observability services."
  type = object({
    nats       = string
    loki       = string
    alloy      = string
    prometheus = string
    grafana    = string
    ingress    = string
    certmgr    = string
    gitea      = string
  })
  default = {
    nats       = "2.15.1"
    loki       = "19.2.1"
    alloy      = "1.14.3"
    prometheus = "29.38.0"
    grafana    = "14.0.1"
    ingress    = "4.15.1"
    certmgr    = "v1.21.3"
    gitea      = "12.7.0"
  }
}
