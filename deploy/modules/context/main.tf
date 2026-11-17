locals {
  name_prefix     = "kodema-${var.cluster_name}"
  installation_id = "kodema-${var.cloud}-${var.cluster_name}"
  is_ha           = var.cost_profile == "ha"

  key_adapter_by_cloud = {
    scw = "scw-kms"
    gcp = "gcp-kms"
    aws = "aws-kms"
  }

  key_ref_prefix_by_cloud = {
    scw = "scw-kms:"
    gcp = "gcp-kms:"
    aws = "aws-kms:"
  }

  key_adapter    = local.key_adapter_by_cloud[var.cloud]
  key_ref_prefix = local.key_ref_prefix_by_cloud[var.cloud]

  state_key = "kodema/${var.cloud}/${var.account_or_project_id}/${var.region}/${var.cluster_name}/infrastructure.tfstate"

  tenant_namespaces = {
    for tenant in var.tenants :
    tenant => "kodema-tenant-${tenant}"
  }

  common_labels = merge(
    {
      "app"            = "kodema"
      "kodema-cloud"   = var.cloud
      "kodema-cluster" = var.cluster_name
      "kodema-env"     = var.environment
      "kodema-profile" = var.cost_profile
      "managed-by"     = "opentofu"
    },
    var.extra_tags
  )

  common_tag_list = [
    for k, v in local.common_labels : "${k}:${v}"
  ]
}
