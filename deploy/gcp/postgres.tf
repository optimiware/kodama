resource "google_sql_database_instance" "main" {
  name                = "${module.context.name_prefix}-pg"
  project             = var.project_id
  region              = var.region
  database_version    = var.postgres_version
  deletion_protection = var.postgres_deletion_protection

  settings {
    tier              = var.postgres_tier
    availability_type = module.context.is_ha ? "REGIONAL" : "ZONAL"
    disk_type         = "PD_SSD"
    disk_size         = var.postgres_disk_size_gb
    disk_autoresize   = true

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
      transaction_log_retention_days = 7

      backup_retention_settings {
        retained_backups = var.postgres_backup_retention_count
        retention_unit   = "COUNT"
      }
    }

    user_labels = module.context.common_labels
  }

  depends_on = [
    google_project_service.required,
    google_service_networking_connection.private_vpc_connection,
  ]
}

resource "google_sql_database" "kodama" {
  name     = "kodama"
  project  = var.project_id
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "admin" {
  name                = var.postgres_admin_user
  project             = var.project_id
  instance            = google_sql_database_instance.main.name
  password_wo         = var.postgres_admin_password
  password_wo_version = var.postgres_password_wo_version
}
