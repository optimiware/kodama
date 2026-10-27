# One qualification session: an isolated installation of the Kodama chart on the stage cluster, prepared the way
# the stage is (every Secret generated here and pushed into Kubernetes before any chart installs). The root does
# not install the Kodama release: install, upgrade and uninstall are the acts the qualification tests perform.
#
# Isolation per session: its own database on the stage PostgreSQL instance (same migration/runtime roles, granted
# on that database only), control-plane namespace, tenant namespaces (created by the chart), NATS server, signing
# and cursor keys, ingress host and installation identifier. Shared with the stage: the managed instance, the
# object-store bucket (unique prefix), Key Manager keys, the Git server, the image registry and the OIDC issuer.

data "scaleway_rdb_instance" "postgres" {
  count = var.manage_database ? 1 : 0

  name = "${var.stage_contract.installation_id}-pg"
}

data "kubernetes_secret_v1" "stage_runtime" {
  metadata {
    name      = "kodama-runtime"
    namespace = var.stage_namespace
  }
}

data "kubernetes_secret_v1" "stage_migration" {
  metadata {
    name      = "kodama-migration"
    namespace = var.stage_namespace
  }
}

data "kubernetes_secret_v1" "stage_kms" {
  metadata {
    name      = var.stage_contract.kms.scw_credentials_secret
    namespace = var.stage_namespace
  }
}

locals {
  s            = var.session
  ns           = "kodama-q-${local.s}"
  installation = "${var.stage_contract.installation_id}-q-${local.s}"
  database     = "kodama_q_${local.s}"
  pg           = var.stage_contract.postgres

  stage_host = var.stage_contract.network.ingress_host
  domain     = join(".", slice(split(".", local.stage_host), 1, length(split(".", local.stage_host))))
  host       = "${local.s}.${local.domain}"

  tenants = [
    { namespace = "${local.ns}-a", tenant_id = "t_${local.s}_a" },
    { namespace = "${local.ns}-b", tenant_id = "t_${local.s}_b" },
  ]

  contract = merge(var.stage_contract, {
    installation_id = local.installation
    postgres        = merge(local.pg, { database = local.database })
    object_storage  = merge(var.stage_contract.object_storage, { artifacts_prefix = "qualification/${local.s}/" })
    network = merge(var.stage_contract.network, {
      ingress_host = local.host
      runtime_url  = "https://${local.host}"
    })
    tenants = local.tenants
  })

  runtime_role_password   = regex("^postgres://[^:]+:([^@]+)@", data.kubernetes_secret_v1.stage_runtime.data["database-runtime-url"])[0]
  migration_role_password = regex("^postgres://[^:]+:([^@]+)@", data.kubernetes_secret_v1.stage_migration.data["database-migration-url"])[0]

  relay_host = "kq-pgrelay.${local.ns}.svc.cluster.local"
  probe_host = "kq-pgprobe.${local.ns}.svc.cluster.local"

  dsn = {
    runtime   = "postgres://${local.pg.runtime_role}:${local.runtime_role_password}@${local.relay_host}:${local.pg.port}/${local.database}?sslmode=${local.pg.ssl_mode}"
    migration = "postgres://${local.pg.migration_role}:${local.migration_role_password}@${local.relay_host}:${local.pg.port}/${local.database}?sslmode=${local.pg.ssl_mode}"
  }

  git_origin = "https://${split("/", trimprefix(var.git_url, "https://"))[0]}/"
  nats_url   = "nats://kodama-nats.${local.ns}.svc.cluster.local:4222"

  kodama_values = templatefile("${path.module}/../shared/stack/values/kodama.yaml.tftpl", {
    contract       = local.contract
    nats_url       = local.nats_url
    profile        = "development"
    kodama_image   = var.kodama_image
    workload_image = var.example_workload_image
    git_url        = var.git_url
    registry_url   = var.registry_url
    oidc_issuer    = var.oidc_issuer
    oidc_audience  = var.oidc_audience
    custom_ca      = false
  })
}

# ---- database ----

resource "scaleway_rdb_database" "session" {
  count = var.manage_database ? 1 : 0

  instance_id = data.scaleway_rdb_instance.postgres[0].id
  name        = local.database
}

resource "scaleway_rdb_privilege" "migration" {
  count = var.manage_database ? 1 : 0

  instance_id   = data.scaleway_rdb_instance.postgres[0].id
  user_name     = local.pg.migration_role
  database_name = scaleway_rdb_database.session[0].name
  permission    = "all"
}

resource "scaleway_rdb_privilege" "runtime" {
  count = var.manage_database ? 1 : 0

  instance_id   = data.scaleway_rdb_instance.postgres[0].id
  user_name     = local.pg.runtime_role
  database_name = scaleway_rdb_database.session[0].name
  permission    = "readwrite"
}

# A session prepared before manage_database existed keeps its database: the resources moved to indexed addresses.
moved {
  from = scaleway_rdb_database.session
  to   = scaleway_rdb_database.session[0]
}

moved {
  from = scaleway_rdb_privilege.migration
  to   = scaleway_rdb_privilege.migration[0]
}

moved {
  from = scaleway_rdb_privilege.runtime
  to   = scaleway_rdb_privilege.runtime[0]
}

# ---- namespace, keys and Secrets ----

resource "kubernetes_namespace_v1" "control_plane" {
  metadata {
    name   = local.ns
    labels = { "kodama.optimi.com/qualification-session" = local.s }
  }
}

resource "tls_private_key" "runtime_signing" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "random_password" "nats_token" {
  length  = 48
  special = false
}

resource "random_bytes" "cursor_key" {
  length = 32
}

resource "random_bytes" "idempotency_hash_key" {
  length = 32
}

resource "kubernetes_secret_v1" "runtime" {
  metadata {
    name      = "kodama-runtime"
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
  }
  data = {
    "database-runtime-url" = local.dsn.runtime
    "nats-token"           = random_password.nats_token.result
    "git-read-token"       = data.kubernetes_secret_v1.stage_runtime.data["git-read-token"]
    "git-write-token"      = data.kubernetes_secret_v1.stage_runtime.data["git-write-token"]
    "runtime-signing-key"  = tls_private_key.runtime_signing.private_key_pem_pkcs8
    "runtime-ca.crt"       = data.kubernetes_secret_v1.stage_runtime.data["runtime-ca.crt"]
    "s3-access-key"        = data.kubernetes_secret_v1.stage_runtime.data["s3-access-key"]
    "s3-secret-key"        = data.kubernetes_secret_v1.stage_runtime.data["s3-secret-key"]
    "git-credentials.json" = jsonencode([
      {
        ref          = "secret://installation/checkout"
        username     = "git"
        passwordFile = "git-read-token"
        urlPrefix    = local.git_origin
        tenants      = [for t in local.tenants : t.tenant_id]
      },
      {
        ref          = "secret://installation/publisher"
        username     = "git"
        passwordFile = "git-write-token"
        urlPrefix    = local.git_origin
        tenants      = [for t in local.tenants : t.tenant_id]
      },
    ])
  }
  binary_data = {
    "cursor-key"           = random_bytes.cursor_key.base64
    "idempotency-hash-key" = random_bytes.idempotency_hash_key.base64
  }
}

resource "kubernetes_secret_v1" "migration" {
  metadata {
    name      = "kodama-migration"
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
  }
  data = {
    "database-migration-url" = local.dsn.migration
  }
}

resource "kubernetes_secret_v1" "nats_auth" {
  metadata {
    name      = "kodama-nats-auth"
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
  }
  data = {
    "nats-token" = random_password.nats_token.result
  }
}

resource "kubernetes_secret_v1" "kms_scw" {
  metadata {
    name      = var.stage_contract.kms.scw_credentials_secret
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
  }
  data = {
    "secret-key" = data.kubernetes_secret_v1.stage_kms.data["secret-key"]
  }
}

# Direct owner/runtime connection strings for the test process, which reaches the private database through the
# probe relay (kubectl port-forward). The server never uses the probe: its path is the faultable relay.
resource "kubernetes_secret_v1" "probe" {
  metadata {
    name      = "kq-probe"
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
  }
  data = {
    "owner-url"   = replace(local.dsn.migration, local.relay_host, local.probe_host)
    "runtime-url" = replace(local.dsn.runtime, local.relay_host, local.probe_host)
  }
}

# ---- NATS (one server per session: the stream name is fixed by the product) ----

resource "helm_release" "nats" {
  name            = "kodama-nats"
  repository      = "https://nats-io.github.io/k8s/helm/charts/"
  chart           = "nats"
  version         = var.nats_chart_version
  namespace       = kubernetes_namespace_v1.control_plane.metadata[0].name
  atomic          = true
  cleanup_on_fail = true
  timeout         = 300
  values          = [templatefile("${path.module}/../shared/stack/values/nats.yaml.tftpl", { namespace = local.ns })]
  depends_on      = [kubernetes_secret_v1.nats_auth]
}

# ---- database relays ----
# kq-pgrelay is the server's path: scaling it to zero severs its connections and refuses new ones (a real outage
# with the database untouched). kq-pgprobe is the test process's path and is never faulted.

resource "kubernetes_deployment_v1" "relay" {
  for_each = { pgrelay = local.relay_host, pgprobe = local.probe_host }

  metadata {
    name      = "kq-${each.key}"
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
    labels    = { "app.kubernetes.io/name" = "kq-${each.key}" }
  }

  spec {
    replicas = 1
    selector {
      match_labels = { "app.kubernetes.io/name" = "kq-${each.key}" }
    }
    template {
      metadata {
        labels = { "app.kubernetes.io/name" = "kq-${each.key}" }
      }
      spec {
        automount_service_account_token = false
        enable_service_links            = false
        security_context {
          run_as_non_root = true
          run_as_user     = 65534
          run_as_group    = 65534
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }
        container {
          name  = "socat"
          image = var.relay_image
          args  = ["-d", "TCP-LISTEN:${local.pg.port},fork,reuseaddr", "TCP:${local.pg.host}:${local.pg.port}"]
          port {
            name           = "pg"
            container_port = local.pg.port
          }
          readiness_probe {
            tcp_socket {
              port = "pg"
            }
            period_seconds = 3
          }
          resources {
            requests = { cpu = "10m", memory = "16Mi" }
            limits   = { memory = "64Mi" }
          }
          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }
        }
      }
    }
  }
  wait_for_rollout = true
}

resource "kubernetes_service_v1" "relay" {
  for_each = { pgrelay = local.relay_host, pgprobe = local.probe_host }

  metadata {
    name      = "kq-${each.key}"
    namespace = kubernetes_namespace_v1.control_plane.metadata[0].name
  }
  spec {
    selector = { "app.kubernetes.io/name" = "kq-${each.key}" }
    port {
      name        = "pg"
      port        = local.pg.port
      target_port = "pg"
    }
  }
}
