variable "kubeconfig_path" {
  description = "Kubeconfig of the stage cluster."
  type        = string
}

variable "project_id" {
  description = "Scaleway project that owns the stage database instance."
  type        = string
}

variable "session" {
  description = "Short qualification session name (lowercase letters and digits). Every resource of the session carries it."
  type        = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9]{0,9}$", var.session))
    error_message = "session must be 1-10 lowercase letters or digits, starting with a letter."
  }
}

variable "stage_contract" {
  description = "The stage_contract of the installation being qualified; the session derives its own contract from it."
  type        = any
}

variable "stage_namespace" {
  description = "Control-plane namespace of the stage installation whose Secrets the session copies (Git tokens, object-store and KMS credentials, database role passwords)."
  type        = string
  default     = "kodama-system"
}

variable "kodama_image" {
  type = object({
    repository = string
    tag        = string
    digest     = string
  })
}

variable "example_workload_image" {
  type = object({
    repository = string
    tag        = string
    digest     = string
  })
}

variable "git_url" {
  description = "HTTPS Git repository endpoint of the stage Git server (any repository of it; its origin is what matters)."
  type        = string
}

variable "registry_url" {
  type = string
}

variable "oidc_issuer" {
  type = string
}

variable "oidc_audience" {
  type = string
}

variable "nats_chart_version" {
  type    = string
  default = "2.15.1"
}

variable "relay_image" {
  description = "Digest-pinned socat image of the database relays."
  type        = string
  default     = "docker.io/alpine/socat:1.8.0.1@sha256:87d88ff79bf42ca723fb360e9585c40e2923a2f4df9b9d148b127568ef6b99e7"
}
