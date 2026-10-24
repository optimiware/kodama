output "stage_contract" {
  description = "Normalized non-secret cloud contract consumed by deploy/stage/shared/stack and gate scripts."
  value = {
    cloud            = "gcp"
    installation_id  = var.installation_id
    region           = var.region
    environment_tier = var.environment_tier

    cluster = {
      name           = google_container_cluster.main.name
      endpoint       = "https://${google_container_cluster.main.endpoint}"
      ca_certificate = google_container_cluster.main.master_auth[0].cluster_ca_certificate
      oidc_issuer    = "https://container.googleapis.com/v1/${google_container_cluster.main.id}"
    }

    postgres = {
      host                  = google_sql_database_instance.postgres.private_ip_address
      port                  = 5432
      database              = google_sql_database.kodama.name
      migration_role        = "kodama_migration"
      runtime_role          = "kodama_runtime"
      ssl_mode              = "require"
      pitr_enabled          = true
      backup_retention_days = local.pitr_days
      rpo_minutes_estimate  = 5
      qualifies_o11_pitr    = local.sql_edition == "ENTERPRISE_PLUS" && local.pitr_days >= 30
    }

    kms = {
      provider = "gcp"
      kek_id   = var.installation_id
      current  = "v1"
      resources = {
        v1 = google_kms_crypto_key.kek_v1.id
        v2 = google_kms_crypto_key.kek_v2.id
      }
      aws_region             = ""
      scw_credentials_secret = ""
    }

    object_storage = {
      endpoint         = "https://storage.googleapis.com"
      region           = var.region
      artifacts_bucket = google_storage_bucket.artifacts.name
      artifacts_prefix = "kodama/"
      logs_bucket      = google_storage_bucket.logs.name
      logs_prefix      = "loki/"
      path_style       = false
    }

    identities = {
      kodama_server_sa_annotations = {
        "iam.gke.io/gcp-service-account" = google_service_account.kodama_server.email
      }
      loki_sa_annotations = {}
    }

    network = {
      runtime_url             = "https://${var.ingress_host}"
      ingress_host            = var.ingress_host
      runtime_egress_cidrs    = var.runtime_callback_cidrs
      dependency_egress_rules = var.dependency_egress_rules
    }

    tenants = var.tenants
  }
}

output "stage_bootstrap_secrets" {
  description = "Sensitive bootstrap material consumed only by deploy/stage/scripts/bootstrap.sh."
  sensitive   = true
  value = {
    postgres_admin_username = google_sql_user.admin.name
    postgres_admin_password = random_password.postgres_admin.result
    artifacts_s3_access_key = google_storage_hmac_key.artifacts_s3.access_id
    artifacts_s3_secret_key = google_storage_hmac_key.artifacts_s3.secret
    logs_s3_access_key      = google_storage_hmac_key.logs_s3.access_id
    logs_s3_secret_key      = google_storage_hmac_key.logs_s3.secret
    scw_kms_secret_key      = ""
  }
}
