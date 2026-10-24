output "stage_contract" {
  description = "Normalized non-secret cloud contract consumed by deploy/stage/shared/stack and gate scripts."
  value = {
    cloud            = "scw"
    installation_id  = var.installation_id
    region           = var.region
    environment_tier = var.environment_tier

    cluster = {
      name           = scaleway_k8s_cluster.main.name
      endpoint       = scaleway_k8s_cluster.main.apiserver_url
      ca_certificate = nonsensitive(scaleway_k8s_cluster.main.kubeconfig[0].cluster_ca_certificate)
      oidc_issuer    = ""
    }

    postgres = {
      host           = scaleway_rdb_instance.postgres.private_network[0].ip
      port           = scaleway_rdb_instance.postgres.private_network[0].port
      database       = scaleway_rdb_database.kodama.name
      migration_role = "kodama_migration"
      runtime_role   = "kodama_runtime"
      ssl_mode       = "require"
      # Scaleway Managed PostgreSQL uses scheduled backups/snapshots rather than continuous WAL PITR.
      pitr_enabled          = false
      backup_retention_days = var.db_backup_retention_days
      rpo_minutes_estimate  = var.db_backup_frequency_hours * 60
      qualifies_o11_pitr    = false
    }

    kms = {
      provider = "scw"
      kek_id   = var.installation_id
      current  = "v1"
      resources = {
        v1 = scaleway_key_manager_key.kek_v1.id
        v2 = scaleway_key_manager_key.kek_v2.id
      }
      aws_region             = ""
      scw_credentials_secret = "kodama-kms-scw"
    }

    object_storage = {
      endpoint         = "https://s3.${var.region}.scw.cloud"
      region           = var.region
      artifacts_bucket = scaleway_object_bucket.artifacts.name
      artifacts_prefix = "kodama/"
      logs_bucket      = scaleway_object_bucket.logs.name
      logs_prefix      = "loki/"
      path_style       = false
    }

    identities = {
      kodama_server_sa_annotations = {}
      loki_sa_annotations          = {}
    }

    network = {
      runtime_url             = "https://${var.ingress_host}"
      ingress_host            = var.ingress_host
      runtime_egress_cidrs    = var.runtime_callback_cidrs
      dependency_egress_rules = var.dependency_egress_rules
    }

    tenants = var.tenants

    images = {
      registry_endpoint = scaleway_registry_namespace.images.endpoint
    }
  }
}

output "stage_bootstrap_secrets" {
  description = "Sensitive bootstrap material consumed only by deploy/stage/scripts/bootstrap.sh."
  sensitive   = true
  value = {
    postgres_admin_username = scaleway_rdb_instance.postgres.user_name
    postgres_admin_password = random_password.postgres_admin.result
    migration_password      = random_password.db_migration.result
    runtime_password        = random_password.db_runtime.result
    artifacts_s3_access_key = scaleway_iam_api_key.artifacts_s3.access_key
    artifacts_s3_secret_key = scaleway_iam_api_key.artifacts_s3.secret_key
    logs_s3_access_key      = scaleway_iam_api_key.logs_s3.access_key
    logs_s3_secret_key      = scaleway_iam_api_key.logs_s3.secret_key
    scw_kms_secret_key      = scaleway_iam_api_key.kodama_kms.secret_key
  }
}
