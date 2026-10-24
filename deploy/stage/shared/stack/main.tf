locals {
  nats_url = "tls://kodama-nats.${var.control_plane_namespace}.svc.cluster.local:4222"
  loki_url = "http://kodama-loki.${var.observability_namespace}.svc.cluster.local:3100"
  prom_url = "http://kodama-prometheus-server.${var.observability_namespace}.svc.cluster.local:80"

  monitored_namespaces = concat(
    [var.control_plane_namespace],
    [for t in var.stage_contract.tenants : t.namespace]
  )

  kodama_values = templatefile("${path.module}/values/kodama.yaml.tftpl", {
    contract       = var.stage_contract
    nats_url       = local.nats_url
    kodama_image   = var.kodama_image
    workload_image = var.example_workload_image
    git_url        = coalesce(var.git_url, "https://git.${var.stage_contract.network.ingress_host}/optimi/kodama-source.git")
    registry_url   = coalesce(var.registry_url, "https://registry.${var.stage_contract.network.ingress_host}")
    oidc_issuer    = coalesce(var.oidc_issuer, "https://auth.${var.stage_contract.network.ingress_host}")
    oidc_audience  = coalesce(var.oidc_audience, "https://${var.stage_contract.network.ingress_host}/api")
    custom_ca      = var.custom_ca
  })

  nats_values = templatefile("${path.module}/values/nats.yaml.tftpl", {
    namespace = var.control_plane_namespace
  })

  loki_values = templatefile("${path.module}/values/loki.yaml.tftpl", {
    object_storage = var.stage_contract.object_storage
    sa_annotations = var.stage_contract.identities.loki_sa_annotations
  })

  alloy_values = templatefile("${path.module}/values/alloy.yaml.tftpl", {
    loki_push_url        = "${local.loki_url}/loki/api/v1/push"
    monitored_namespaces = local.monitored_namespaces
  })

  prometheus_values = templatefile("${path.module}/values/prometheus.yaml.tftpl", {
    control_plane_namespace = var.control_plane_namespace
    observability_namespace = var.observability_namespace
  })

  grafana_values = templatefile("${path.module}/values/grafana.yaml.tftpl", {
    loki_url     = local.loki_url
    prom_url     = local.prom_url
    ingress_host = "grafana.${var.stage_contract.network.ingress_host}"
  })
}

resource "helm_release" "nats" {
  name             = "kodama-nats"
  repository       = "https://nats-io.github.io/k8s/helm/charts/"
  chart            = "nats"
  version          = var.chart_versions.nats
  namespace        = var.control_plane_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.nats_values]
}

resource "helm_release" "loki" {
  name             = "kodama-loki"
  repository       = "https://grafana.github.io/helm-charts"
  chart            = "loki"
  version          = var.chart_versions.loki
  namespace        = var.observability_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.loki_values]
}

resource "helm_release" "alloy" {
  name             = "kodama-alloy"
  repository       = "https://grafana.github.io/helm-charts"
  chart            = "alloy"
  version          = var.chart_versions.alloy
  namespace        = var.observability_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.alloy_values]
  depends_on       = [helm_release.loki]
}

resource "helm_release" "prometheus" {
  name             = "kodama-prometheus"
  repository       = "https://prometheus-community.github.io/helm-charts"
  chart            = "prometheus"
  version          = var.chart_versions.prometheus
  namespace        = var.observability_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.prometheus_values]
}

resource "helm_release" "grafana" {
  name             = "kodama-grafana"
  repository       = "https://grafana.github.io/helm-charts"
  chart            = "grafana"
  version          = var.chart_versions.grafana
  namespace        = var.observability_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.grafana_values]
  depends_on       = [helm_release.loki, helm_release.prometheus]
}

resource "helm_release" "kodama" {
  name             = "kodama"
  chart            = "${path.module}/../../../helm/kodama"
  namespace        = var.control_plane_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.kodama_values]
  depends_on       = [helm_release.nats]
}
