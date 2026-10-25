terraform {
  required_version = ">= 1.8.0"

  # Partial configuration: qualify.sh passes -backend-config=path=$STAGE_STATE_DIR/qualification/<session>.tfstate so
  # state (database passwords, generated keys) lives outside the checkout, one state per session.
  backend "local" {}

  required_providers {
    scaleway = {
      source  = "scaleway/scaleway"
      version = "~> 2.88"
    }
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
  }
}

provider "scaleway" {
  project_id = var.project_id
  region     = var.stage_contract.region
  zone       = "${var.stage_contract.region}-1"
}

provider "helm" {
  kubernetes = {
    config_path = var.kubeconfig_path
  }
}

provider "kubernetes" {
  config_path = var.kubeconfig_path
}
