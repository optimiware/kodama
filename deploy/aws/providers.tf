locals {
  effective_azs = length(var.availability_zones) >= 2 ? var.availability_zones : ["${var.region}a", "${var.region}b"]
}

provider "aws" {
  region = var.region

  default_tags {
    tags = module.context.common_labels
  }
}

module "context" {
  source = "../modules/context"

  cloud                 = "aws"
  cluster_name          = var.cluster_name
  environment           = var.environment
  cost_profile          = var.cost_profile
  account_or_project_id = var.account_id
  region                = var.region
  zone                  = local.effective_azs[0]
  release_namespace     = var.release_namespace
  tenants               = var.tenants
  extra_tags            = var.extra_tags
}
