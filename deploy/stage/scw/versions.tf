terraform {
  required_version = ">= 1.8.0"

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
