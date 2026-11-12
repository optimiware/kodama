locals {
  effective_zone = var.zone != "" ? var.zone : "${var.region}-1"
}

provider "scaleway" {
  project_id = var.project_id
  region     = var.region
  zone       = local.effective_zone
}

module "context" {
  source = "../modules/context"

  cloud                 = "scw"
  cluster_name          = var.cluster_name
  environment           = var.environment
  cost_profile          = var.cost_profile
  account_or_project_id = var.project_id
  region                = var.region
  zone                  = local.effective_zone
  release_namespace     = var.release_namespace
  tenants               = var.tenants
  extra_tags            = var.extra_tags
}
