resource "random_id" "keyring_suffix" {
  byte_length = 4
}

resource "google_kms_key_ring" "kodama" {
  name     = "${module.context.name_prefix}-kr-${random_id.keyring_suffix.hex}"
  project  = var.project_id
  location = var.region

  depends_on = [google_project_service.required]
}

resource "google_kms_crypto_key" "kodama" {
  name            = "${module.context.name_prefix}-kek"
  key_ring        = google_kms_key_ring.kodama.id
  purpose         = "ENCRYPT_DECRYPT"
  rotation_period = var.kms_rotation_period
  labels          = module.context.common_labels

  version_template {
    algorithm        = "GOOGLE_SYMMETRIC_ENCRYPTION"
    protection_level = "SOFTWARE"
  }
}

resource "google_service_account" "kodama_controller" {
  account_id   = "${module.context.name_prefix}-ctrl"
  project      = var.project_id
  display_name = "Kodama controller service account (${module.context.cluster_name})"

  depends_on = [google_project_service.required]
}

resource "google_kms_crypto_key_iam_member" "kodama_controller" {
  crypto_key_id = google_kms_crypto_key.kodama.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  member        = "serviceAccount:${google_service_account.kodama_controller.email}"
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.kodama_controller.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.release_namespace}/kodama]"

  depends_on = [google_container_cluster.main]
}
