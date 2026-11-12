provider "google" {
  project = var.project_id
  region  = var.region
}

resource "google_storage_bucket" "tofu_state" {
  name                        = var.bucket_name
  project                     = var.project_id
  location                    = upper(var.region)
  force_destroy               = var.force_destroy
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  versioning {
    enabled = true
  }

  lifecycle_rule {
    action {
      type = "Delete"
    }
    condition {
      num_newer_versions         = 10
      days_since_noncurrent_time = var.noncurrent_version_expiration_days
    }
  }

  labels = {
    app        = "kodama"
    component  = "tofu-state"
    managed-by = "opentofu"
  }
}
