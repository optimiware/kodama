locals {
  name_prefix   = var.installation_id
  sql_edition   = var.environment_tier == "production" ? "ENTERPRISE_PLUS" : "ENTERPRISE"
  pitr_days     = var.environment_tier == "production" ? max(var.db_pitr_retention_days, 30) : min(var.db_pitr_retention_days, 7)
  workload_pool = "${var.project_id}.svc.id.goog"
}

# --- Dedicated VPC & Private Service Access ---

resource "google_compute_network" "main" {
  name                    = "${local.name_prefix}-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "main" {
  name                     = "${local.name_prefix}-subnet"
  region                   = var.region
  network                  = google_compute_network.main.id
  ip_cidr_range            = var.subnet_cidr
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = var.pods_cidr
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = var.services_cidr
  }
}

resource "google_compute_global_address" "private_service_range" {
  name          = "${local.name_prefix}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 16
  network       = google_compute_network.main.id
}

resource "google_service_networking_connection" "private_vpc_connection" {
  network                 = google_compute_network.main.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_service_range.name]
}

# --- Managed GKE Cluster (Dataplane V2 enforces NetworkPolicy) ---

resource "google_container_cluster" "main" {
  name                     = local.name_prefix
  location                 = var.region
  network                  = google_compute_network.main.name
  subnetwork               = google_compute_subnetwork.main.name
  remove_default_node_pool = true
  initial_node_count       = 1
  datapath_provider        = "ADVANCED_DATAPATH"
  deletion_protection      = var.environment_tier == "production"

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  workload_identity_config {
    workload_pool = local.workload_pool
  }
}

resource "google_container_node_pool" "workers" {
  name       = "${local.name_prefix}-workers"
  location   = var.region
  cluster    = google_container_cluster.main.name
  node_count = var.node_count

  node_config {
    machine_type = var.node_machine_type
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

# --- Cloud SQL for PostgreSQL ---

resource "random_password" "postgres_admin" {
  length  = 32
  special = false
}

resource "google_sql_database_instance" "postgres" {
  name                = "${local.name_prefix}-pg"
  region              = var.region
  database_version    = "POSTGRES_16"
  deletion_protection = var.environment_tier == "production"

  settings {
    tier              = var.db_tier
    edition           = local.sql_edition
    availability_type = var.environment_tier == "production" ? "REGIONAL" : "ZONAL"
    disk_autoresize   = true
    disk_size         = 50
    disk_type         = "PD_SSD"

    ip_configuration {
      ipv4_enabled                                  = false
      private_network                               = google_compute_network.main.id
      enable_private_path_for_google_cloud_services = true
      ssl_mode                                      = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "02:00"
      transaction_log_retention_days = local.pitr_days
      backup_retention_settings {
        retained_backups = local.pitr_days
        retention_unit   = "COUNT"
      }
    }
  }

  depends_on = [google_service_networking_connection.private_vpc_connection]
}

resource "google_sql_database" "kodama" {
  name     = "kodama"
  instance = google_sql_database_instance.postgres.name
}

resource "google_sql_user" "admin" {
  name     = "kodama_admin"
  instance = google_sql_database_instance.postgres.name
  password = random_password.postgres_admin.result
}

# --- Cloud KMS (Scope-Key Wrapping, Current + Retained Rotation Key) ---

resource "google_kms_key_ring" "kodama" {
  name     = "${local.name_prefix}-keyring"
  location = var.region
}

resource "google_kms_crypto_key" "kek_v1" {
  name            = "${local.name_prefix}-kek-v1"
  key_ring        = google_kms_key_ring.kodama.id
  purpose         = "ENCRYPT_DECRYPT"
  rotation_period = "2592000s"
}

resource "google_kms_crypto_key" "kek_v2" {
  name            = "${local.name_prefix}-kek-v2"
  key_ring        = google_kms_key_ring.kodama.id
  purpose         = "ENCRYPT_DECRYPT"
  rotation_period = "2592000s"
}

resource "google_service_account" "kodama_server" {
  account_id   = substr("${local.name_prefix}-srv", 0, 28)
  display_name = "Kodama server workload identity (${local.name_prefix})"
}

resource "google_kms_crypto_key_iam_member" "server_kek_v1" {
  crypto_key_id = google_kms_crypto_key.kek_v1.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${google_service_account.kodama_server.email}"
}

resource "google_kms_crypto_key_iam_member" "server_kek_v2" {
  crypto_key_id = google_kms_crypto_key.kek_v2.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${google_service_account.kodama_server.email}"
}

resource "google_service_account_iam_member" "server_wi" {
  service_account_id = google_service_account.kodama_server.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${local.workload_pool}[kodama-system/kodama]"
}

# --- GCS Buckets + S3-Compatible HMAC Keys (Artifacts vs Loki Logs) ---

resource "google_storage_bucket" "artifacts" {
  name                        = "${var.project_id}-${local.name_prefix}-artifacts"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = var.environment_tier != "production"

  versioning {
    enabled = true
  }
}

resource "google_storage_bucket" "logs" {
  name                        = "${var.project_id}-${local.name_prefix}-loki-logs"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = var.environment_tier != "production"
}

resource "google_service_account" "artifacts_s3" {
  account_id   = substr("${local.name_prefix}-art", 0, 28)
  display_name = "Kodama S3 XML interoperability for artifacts"
}

resource "google_storage_bucket_iam_member" "artifacts_s3" {
  bucket = google_storage_bucket.artifacts.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.artifacts_s3.email}"
}

resource "google_storage_hmac_key" "artifacts_s3" {
  service_account_email = google_service_account.artifacts_s3.email
}

resource "google_service_account" "logs_s3" {
  account_id   = substr("${local.name_prefix}-log", 0, 28)
  display_name = "Loki S3 XML interoperability for logs"
}

resource "google_storage_bucket_iam_member" "logs_s3" {
  bucket = google_storage_bucket.logs.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.logs_s3.email}"
}

resource "google_storage_hmac_key" "logs_s3" {
  service_account_email = google_service_account.logs_s3.email
}
