locals {
  nats_url = "${var.nats_tls ? "tls" : "nats"}://kodama-nats.${var.control_plane_namespace}.svc.cluster.local:4222"
  loki_url = "http://kodama-loki.${var.observability_namespace}.svc.cluster.local:3100"
  prom_url = "http://kodama-prometheus-server.${var.observability_namespace}.svc.cluster.local:80"

  monitored_namespaces = concat(
    [var.control_plane_namespace],
    [for t in var.stage_contract.tenants : t.namespace]
  )
  # Alloy keeps these namespaces (the relabel regex is fully anchored): the control plane, the contract's tenant
  # namespaces and the qualification sessions' namespaces.
  monitored_namespace_regex = join("|", concat(local.monitored_namespaces, ["kodama-q-[a-z0-9]+(-[a-z0-9]+)?"]))

  kodama_values = templatefile("${path.module}/values/kodama.yaml.tftpl", {
    contract      = var.stage_contract
    nats_url      = local.nats_url
    kodama_image  = var.kodama_image
    oidc_issuer   = coalesce(var.oidc_issuer, "https://auth.${var.stage_contract.network.ingress_host}")
    oidc_audience = coalesce(var.oidc_audience, "https://${var.stage_contract.network.ingress_host}/api")
    storage_class = var.storage_class
  })

  nats_values = templatefile("${path.module}/values/nats.yaml.tftpl", {
    namespace = var.control_plane_namespace
  })

  loki_values = templatefile("${path.module}/values/loki.yaml.tftpl", {
    object_storage = var.stage_contract.object_storage
    sa_annotations = var.stage_contract.identities.loki_sa_annotations
  })

  alloy_values = templatefile("${path.module}/values/alloy.yaml.tftpl", {
    loki_push_url             = "${local.loki_url}/loki/api/v1/push"
    monitored_namespace_regex = local.monitored_namespace_regex
  })

  prometheus_values = templatefile("${path.module}/values/prometheus.yaml.tftpl", {
    control_plane_namespace = var.control_plane_namespace
    observability_namespace = var.observability_namespace
  })

  deps_host = var.stage_contract.network.ingress_host

  # The Helm provider does not notice edits inside a local chart directory, so the release description
  # carries a digest of every chart file: a chart change always produces an upgrade.
  kodama_chart_dir    = "${path.module}/../../../charts/kodama-kernel"
  kodama_chart_digest = substr(sha256(join("", [for f in sort(tolist(fileset(local.kodama_chart_dir, "**"))) : "${f}:${filesha256("${local.kodama_chart_dir}/${f}")}" if !endswith(f, "_test.go")])), 0, 16)

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
  depends_on       = [kubernetes_secret_v1.nats_auth]
}

resource "helm_release" "loki" {
  name             = "kodama-loki"
  repository       = "https://grafana-community.github.io/helm-charts"
  chart            = "loki"
  version          = var.chart_versions.loki
  namespace        = var.observability_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.loki_values]
  depends_on       = [kubernetes_secret_v1.loki_s3]
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
  depends_on       = [helm_release.loki, kubernetes_namespace_v1.observability]
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
  depends_on       = [kubernetes_namespace_v1.observability]
}

resource "helm_release" "grafana" {
  name             = "kodama-grafana"
  repository       = "https://grafana-community.github.io/helm-charts"
  chart            = "grafana"
  version          = var.chart_versions.grafana
  namespace        = var.observability_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.grafana_values]
  depends_on       = [helm_release.loki, helm_release.prometheus, kubernetes_secret_v1.grafana_admin, kubernetes_config_map_v1.grafana_dashboards, helm_release.ingress_nginx, helm_release.cluster_issuer]
}

resource "helm_release" "kodama" {
  name             = "kodama"
  chart            = local.kodama_chart_dir
  description      = "kodama chart ${local.kodama_chart_digest}"
  namespace        = var.control_plane_namespace
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [local.kodama_values]
  depends_on       = [helm_release.nats, kubernetes_deployment_v1.oauthsonas, kubernetes_secret_v1.kernel_runtime, kubernetes_secret_v1.kernel_migration, kubernetes_secret_v1.kms_scw, kubernetes_job_v1.bootstrap_sql, helm_release.ingress_nginx, helm_release.cluster_issuer, terraform_data.public_certs]
}

resource "helm_release" "ingress_nginx" {
  name             = "ingress-nginx"
  repository       = "https://kubernetes.github.io/ingress-nginx"
  chart            = "ingress-nginx"
  version          = var.chart_versions.ingress
  namespace        = "ingress-nginx"
  create_namespace = true
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values           = [file("${path.module}/values/ingress-nginx.yaml")]
}

resource "helm_release" "cert_manager" {
  name             = "cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = var.chart_versions.certmgr
  namespace        = "cert-manager"
  create_namespace = true
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 300
  values = [yamlencode({
    crds = { enabled = true }
  })]
}

# Let's Encrypt ClusterIssuer (HTTP-01 through the nginx ingress), installed as a local chart so the apply
# does not need the cert-manager CRDs at plan time.
resource "helm_release" "cluster_issuer" {
  name             = "kodama-cluster-issuer"
  chart            = "${path.module}/../../../charts/cluster-issuer"
  namespace        = "cert-manager"
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 120
  values = [yamlencode({
    acmeEmail    = var.acme_email
    acmeServer   = var.acme_server
    ingressClass = "nginx"
  })]
  depends_on = [helm_release.cert_manager, helm_release.ingress_nginx]
}

# The OIDC issuer must present a publicly trusted certificate before the server's readiness probe runs.
resource "terraform_data" "public_certs" {
  triggers_replace = [
    var.oauthsonas_image,
    helm_release.grafana.version,
    filesha256("${path.module}/../../scripts/wait-certs.sh"),
  ]

  provisioner "local-exec" {
    command = "${path.module}/../../scripts/wait-certs.sh"
    environment = {
      KUBECONFIG = var.kubeconfig_path
      KODAMA_WAIT_CERTS = join(",", [
        "${var.deps_namespace}/kodama-oauthsonas-tls",
        "${var.observability_namespace}/kodama-grafana-tls",
      ])
    }
  }
}
