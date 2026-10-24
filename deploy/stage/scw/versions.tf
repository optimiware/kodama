terraform {
  required_version = ">= 1.8.0"

  # Partial configuration: deploy.sh passes -backend-config=path=$STAGE_STATE_DIR/<root>.tfstate so state
  # (which holds database and API-key secrets) lives outside the checkout.
  backend "local" {}

  required_providers {
    scaleway = {
      source  = "scaleway/scaleway"
      version = "~> 2.88"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}

provider "scaleway" {
  project_id = var.project_id
  region     = var.region
  zone       = "${var.region}-1"
}
