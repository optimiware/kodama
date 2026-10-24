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
  description = "Whether the runtime and migration Secrets carry ca.crt for internal TLS verification."
  type        = bool
  default     = true
}

variable "chart_versions" {
  description = "Pinned upstream Helm chart versions for shared backend and observability services."
  type = object({
    nats       = string
    loki       = string
    alloy      = string
    prometheus = string
    grafana    = string
  })
  default = {
    nats       = "2.15.1"
    loki       = "19.2.1"
    alloy      = "1.14.3"
    prometheus = "29.38.0"
    grafana    = "14.0.1"
  }
}
