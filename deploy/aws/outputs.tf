locals {
  kms_key_ref = "${module.context.key_ref_prefix}${aws_kms_key.kodama.arn}"

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
      cluster_id            = aws_eks_cluster.main.id
      cluster_name          = aws_eks_cluster.main.name
      apiserver_url         = aws_eks_cluster.main.endpoint
      cni                   = "amazon-vpc-cni-networkpolicy"
      storage_class         = var.workspace_storage_class
      service_account_name  = "kodama"
      workload_identity_ref = aws_iam_role.kodama_controller.arn
    }
    postgres = {
      instance_id   = aws_db_instance.main.id
      instance_name = aws_db_instance.main.identifier
      database_name = aws_db_instance.main.db_name
      host          = aws_db_instance.main.address
      port          = aws_db_instance.main.port
      admin_user    = var.postgres_admin_user
      ssl_mode      = "verify-full"
    }
    kms = {
      key_adapter  = module.context.key_adapter
      key_id       = aws_kms_key.kodama.arn
      key_ref      = local.kms_key_ref
      iam_identity = aws_iam_role.kodama_controller.arn
    }
  }
}

output "deployment_descriptor" {
  description = "Normalized multi-cloud deployment descriptor consumed by scripts/deploy and scripts/bootstrap-secrets."
  value       = local.deployment_descriptor
}

output "database_ca_certificate" {
  description = "Operator-supplied public Amazon RDS CA certificate PEM bundle, mounted as ca.crt by scripts/bootstrap-secrets."
  value       = var.database_ca_bundle
  sensitive   = true
}

output "kms_key_ref" {
  description = "Kodama KMS key reference string."
  value       = local.kms_key_ref
}
