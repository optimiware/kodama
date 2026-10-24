terraform {
  required_version = ">= 1.8.0"

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
