locals {
  effective_zone = var.zone != "" ? var.zone : "${var.region}-b"

  required_apis = toset([
    "compute.googleapis.com",
    "container.googleapis.com",
    "sqladmin.googleapis.com",
    "cloudkms.googleapis.com",
    "servicenetworking.googleapis.com",
    "iam.googleapis.com",
  ])
}

provider "google" {
  project = var.project_id
  region  = var.region
  zone    = local.effective_zone
}

resource "google_project_service" "required" {
  for_each = var.enable_apis ? local.required_apis : toset([])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

module "context" {
  source = "../modules/context"

  cloud                 = "gcp"
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
