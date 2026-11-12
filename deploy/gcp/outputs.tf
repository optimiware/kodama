locals {
  kms_key_ref = "${module.context.key_ref_prefix}${google_kms_crypto_key.kodama.id}"

  deployment_descriptor = {
    cloud                 = module.context.cloud
    cluster_name          = module.context.cluster_name
    installation_id       = module.context.installation_id
    environment           = module.context.environment
    cost_profile          = module.context.cost_profile
    account_or_project_id = module.context.account_or_project_id
    region                = module.context.region
    zone                  = module.context.zone
    release_namespace     = module.context.release_namespace
    tenants               = module.context.tenants
    tenant_namespaces     = module.context.tenant_namespaces
    kubernetes = {
      cluster_id            = google_container_cluster.main.id
      cluster_name          = google_container_cluster.main.name
      apiserver_url         = "https://${google_container_cluster.main.endpoint}"
      cni                   = "cilium-dataplane-v2"
      storage_class         = var.workspace_storage_class
      service_account_name  = "kodama"
      workload_identity_ref = google_service_account.kodama_controller.email
    }
    postgres = {
      instance_id   = google_sql_database_instance.main.id
      instance_name = google_sql_database_instance.main.name
      database_name = google_sql_database.kodama.name
      host          = google_sql_database_instance.main.private_ip_address
      port          = 5432
      admin_user    = var.postgres_admin_user
      ssl_mode      = "verify-full"
    }
    kms = {
      key_adapter  = module.context.key_adapter
      key_id       = google_kms_crypto_key.kodama.id
      key_ref      = local.kms_key_ref
      iam_identity = google_service_account.kodama_controller.email
    }
  }
}

output "deployment_descriptor" {
  description = "Normalized multi-cloud deployment descriptor consumed by scripts/deploy and scripts/bootstrap-secrets."
  value       = local.deployment_descriptor
}

output "database_ca_certificate" {
  description = "PEM-encoded server CA certificate for the Cloud SQL PostgreSQL instance."
  value       = try(google_sql_database_instance.main.server_ca_cert[0].cert, "")
  sensitive   = true
}

output "kms_key_ref" {
  description = "Kodama KMS key reference string."
  value       = local.kms_key_ref
}
