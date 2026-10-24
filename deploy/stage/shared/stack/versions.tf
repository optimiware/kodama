terraform {
  required_version = ">= 1.8.0"

  # Partial configuration: deploy.sh passes -backend-config=path=$STAGE_STATE_DIR/<root>.tfstate so state
  # (which holds database and API-key secrets) lives outside the checkout.
  backend "local" {}

  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.3"
    }
  }
}

provider "helm" {
  kubernetes = {
    config_path = var.kubeconfig_path
  }
}
