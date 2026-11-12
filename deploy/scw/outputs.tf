locals {
  pg_private_endpoint = scaleway_rdb_instance.main.private_network[0]
  pg_host             = coalesce(try(local.pg_private_endpoint.ip, null), local.pg_private_endpoint.hostname)
  pg_port             = local.pg_private_endpoint.port
  kms_key_ref         = "${module.context.key_ref_prefix}${scaleway_key_manager_key.kodama.id}"

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
      cluster_id            = scaleway_k8s_cluster.main.id
      cluster_name          = scaleway_k8s_cluster.main.name
      apiserver_url         = scaleway_k8s_cluster.main.apiserver_url
      cni                   = scaleway_k8s_cluster.main.cni
      storage_class         = var.workspace_storage_class
      service_account_name  = "kodama"
      workload_identity_ref = scaleway_iam_application.kodama_kms.id
    }
    postgres = {
      instance_id    = scaleway_rdb_instance.main.id
      instance_name  = scaleway_rdb_instance.main.name
      database_name  = var.postgres_database_name
      admin_database = var.postgres_admin_database
      host           = local.pg_host
      port           = local.pg_port
      admin_user     = var.postgres_admin_user
      ssl_mode       = "verify-full"
    }
    kms = {
      key_adapter  = module.context.key_adapter
      key_id       = scaleway_key_manager_key.kodama.id
      key_ref      = local.kms_key_ref
      iam_identity = scaleway_iam_application.kodama_kms.id
    }
  }
}

output "deployment_descriptor" {
  description = "Normalized multi-cloud deployment descriptor consumed by scripts/deploy and scripts/bootstrap-secrets."
  value       = local.deployment_descriptor
}

output "kubeconfig_raw" {
  description = "Raw kubeconfig YAML for the Scaleway Kapsule cluster."
  value       = scaleway_k8s_cluster.main.kubeconfig[0].config_file
  sensitive   = true
}

output "database_ca_certificate" {
  description = "PEM-encoded CA certificate for the Scaleway Managed PostgreSQL instance."
  value       = scaleway_rdb_instance.main.certificate
  sensitive   = true
}

output "kms_key_ref" {
  description = "Kodama KMS key reference string."
  value       = local.kms_key_ref
}

output "kms_credentials" {
  description = "Dedicated least-privilege Scaleway IAM application credentials for KMS decrypt."
  value = {
    access_key = scaleway_iam_api_key.kodama_kms.access_key
    secret_key = scaleway_iam_api_key.kodama_kms.secret_key
    project_id = var.project_id
    region     = var.region
  }
  sensitive = true
}
