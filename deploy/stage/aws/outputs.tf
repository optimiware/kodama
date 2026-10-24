output "stage_contract" {
  description = "Normalized non-secret cloud contract consumed by deploy/stage/shared/stack and gate scripts."
  value = {
    cloud            = "aws"
    installation_id  = var.installation_id
    region           = var.region
    environment_tier = var.environment_tier

    cluster = {
      name           = aws_eks_cluster.main.name
      endpoint       = aws_eks_cluster.main.endpoint
      ca_certificate = aws_eks_cluster.main.certificate_authority[0].data
      oidc_issuer    = aws_eks_cluster.main.identity[0].oidc[0].issuer
    }

    postgres = {
      host                  = aws_db_instance.postgres.address
      port                  = aws_db_instance.postgres.port
      database              = aws_db_instance.postgres.db_name
      migration_role        = "kodama_migration"
      runtime_role          = "kodama_runtime"
      ssl_mode              = "require"
      pitr_enabled          = true
      backup_retention_days = local.effective_retention
      rpo_minutes_estimate  = 5
      qualifies_o11_pitr    = local.effective_retention >= 30
    }

    kms = {
      provider = "aws"
      kek_id   = var.installation_id
      current  = "v1"
      resources = {
        v1 = aws_kms_key.kek_v1.arn
        v2 = aws_kms_key.kek_v2.arn
      }
      aws_region             = var.region
      scw_credentials_secret = ""
    }

    object_storage = {
      endpoint         = "https://s3.${var.region}.amazonaws.com"
      region           = var.region
      artifacts_bucket = aws_s3_bucket.artifacts.bucket
      artifacts_prefix = "kodama/"
      logs_bucket      = aws_s3_bucket.logs.bucket
      logs_prefix      = "loki/"
      path_style       = false
    }

    identities = {
      kodama_server_sa_annotations = {
        "eks.amazonaws.com/role-arn" = aws_iam_role.kodama_server.arn
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
    postgres_admin_username = aws_db_instance.postgres.username
    postgres_admin_password = random_password.postgres_admin.result
    artifacts_s3_access_key = aws_iam_access_key.artifacts_s3.id
    artifacts_s3_secret_key = aws_iam_access_key.artifacts_s3.secret
    logs_s3_access_key      = aws_iam_access_key.logs_s3.id
    logs_s3_secret_key      = aws_iam_access_key.logs_s3.secret
    scw_kms_secret_key      = ""
  }
}
