output "session" {
  description = "Nonsecret handle of the session for the test process and the gates."
  value = {
    name            = local.s
    installation_id = local.installation
    namespace       = local.ns
    release         = "kodama"
    host            = local.host
    url             = "https://${local.host}"
    audience        = var.oidc_audience
    issuer          = var.oidc_issuer
    database        = local.database
    tenants         = local.tenants
    nats_service    = "kodama-nats"
    probe_service   = "kq-pgprobe"
    relay_service   = "kq-pgrelay"
    probe_secret    = "kq-probe"
    git_origin      = local.git_origin
  }
}

output "contract" {
  description = "The session's stage_contract (derived from the stage's)."
  value       = local.contract
}

output "kodama_values" {
  description = "Helm values of the session's Kodama release."
  value       = local.kodama_values
}
