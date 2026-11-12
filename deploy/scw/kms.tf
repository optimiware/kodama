resource "scaleway_key_manager_key" "kodama" {
  name        = "${module.context.name_prefix}-kek"
  region      = var.region
  project_id  = var.project_id
  usage       = "symmetric_encryption"
  algorithm   = "aes_256_gcm"
  description = "Kodama envelope encryption key for cluster ${module.context.cluster_name}"
  unprotected = var.kms_unprotected
  tags        = module.context.common_tag_list

  rotation_policy {
    rotation_period = var.kms_rotation_period
  }
}

# Scaleway IAM tags reject ":" (they accept "key=value"), unlike the other resources.
locals {
  iam_tags = [for k, v in module.context.common_labels : "${k}=${v}"]
}

resource "scaleway_iam_application" "kodama_kms" {
  name        = "${module.context.name_prefix}-kms-controller"
  description = "Kodama controller KMS identity for cluster ${module.context.cluster_name}"
  tags        = local.iam_tags
}

resource "scaleway_iam_policy" "kodama_kms" {
  name           = "${module.context.name_prefix}-kms-policy"
  description    = "Least-privilege Scaleway Key Manager decrypt access for Kodama cluster ${module.context.cluster_name}"
  application_id = scaleway_iam_application.kodama_kms.id
  tags           = local.iam_tags

  rule {
    project_ids = [var.project_id]
    permission_set_names = [
      "KeyManagerReadOnly",
      "KeyManagerKeyDecrypt",
    ]
  }
}

resource "scaleway_iam_api_key" "kodama_kms" {
  application_id     = scaleway_iam_application.kodama_kms.id
  default_project_id = var.project_id
  description        = "Dedicated Key Manager decrypt API key for Kodama cluster ${module.context.cluster_name}"

  # The provider reports the expiry the API assigns (one year); unset in
  # configuration it reads as a change that forces replacement on every plan.
  # Rotate before it expires: `tofu apply -replace=scaleway_iam_api_key.kodama_kms`
  # then rerun scripts/bootstrap-secrets (it reapplies kodama-kms-credentials).
  lifecycle {
    ignore_changes = [expires_at]
  }
}
