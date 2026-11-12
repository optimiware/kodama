provider "scaleway" {
  project_id = var.project_id
  region     = var.region
}

resource "scaleway_object_bucket" "tofu_state" {
  name          = var.bucket_name
  region        = var.region
  project_id    = var.project_id
  force_destroy = var.force_destroy

  versioning {
    enabled = true
  }

  lifecycle_rule {
    id      = "expire-noncurrent-state-and-lock-versions"
    enabled = true

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_version_expiration_days
    }

    abort_incomplete_multipart_upload_days = 7
  }

  tags = {
    app        = "kodama"
    component  = "tofu-state"
    managed-by = "opentofu"
  }
}

resource "scaleway_object_bucket_acl" "tofu_state" {
  bucket     = scaleway_object_bucket.tofu_state.id
  region     = var.region
  project_id = var.project_id
  acl        = "private"
}
