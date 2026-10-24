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
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.4"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.4"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
    htpasswd = {
      source  = "loafoe/htpasswd"
      version = "~> 2.1"
    }
  }
}

provider "helm" {
  kubernetes = {
    config_path = var.kubeconfig_path
  }
}

provider "kubernetes" {
  config_path = var.kubeconfig_path
}
