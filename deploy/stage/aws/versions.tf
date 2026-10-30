terraform {
  required_version = ">= 1.8.0"

  # Partial configuration: deploy.sh passes -backend-config=path=$STAGE_STATE_DIR/clusters/<name>/aws.tfstate so state
  # (which holds database and API-key secrets) lives outside the checkout, one file per cluster name.
  backend "local" {}

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.70"
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

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      app             = "kodama"
      installation_id = var.installation_id
      environment     = var.environment_tier
      managed_by      = "opentofu"
    }
  }
}
