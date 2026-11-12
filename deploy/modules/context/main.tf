locals {
  name_prefix     = "kodama-${var.cluster_name}"
  installation_id = "kodama-${var.cloud}-${var.cluster_name}"
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

  state_key = "kodama/${var.cloud}/${var.account_or_project_id}/${var.region}/${var.cluster_name}/infrastructure.tfstate"

  tenant_namespaces = {
    for tenant in var.tenants :
    tenant => "kodama-tenant-${tenant}"
  }

  common_labels = merge(
    {
      "app"            = "kodama"
      "kodama-cloud"   = var.cloud
      "kodama-cluster" = var.cluster_name
      "kodama-env"     = var.environment
      "kodama-profile" = var.cost_profile
      "managed-by"     = "opentofu"
    },
    var.extra_tags
  )

  common_tag_list = [
    for k, v in local.common_labels : "${k}:${v}"
  ]
}
