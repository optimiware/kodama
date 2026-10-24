output "rendered_kodama_values" {
  description = "Rendered Kodama Helm values YAML for offline lint/schema verification and evidence bundles."
  value       = local.kodama_values
}

output "stack_endpoints" {
  description = "Internal and external endpoints for the deployed shared stack."
  value = {
    cloud                   = var.stage_contract.cloud
    installation_id         = var.stage_contract.installation_id
    control_plane_namespace = var.control_plane_namespace
    observability_namespace = var.observability_namespace
    kodama_runtime_url      = var.stage_contract.network.runtime_url
    nats_url                = local.nats_url
    loki_url                = local.loki_url
    prometheus_url          = local.prom_url
    grafana_host            = "grafana.${var.stage_contract.network.ingress_host}"
  }
}
