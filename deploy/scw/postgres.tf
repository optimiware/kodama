resource "scaleway_rdb_instance" "main" {
  name                      = "${module.context.name_prefix}-pg"
  region                    = var.region
  project_id                = var.project_id
  engine                    = var.postgres_engine
  node_type                 = var.postgres_node_type
  is_ha_cluster             = module.context.is_ha
  encryption_at_rest        = true
  volume_type               = "sbs_5k"
  volume_size_in_gb         = var.postgres_volume_size_gb
  user_name                 = var.postgres_admin_user
  password_wo               = var.postgres_admin_password
  password_wo_version       = var.postgres_password_wo_version
  disable_backup            = false
  backup_schedule_frequency = var.postgres_backup_frequency_hours
  backup_schedule_retention = var.postgres_backup_retention_days
  backup_same_region        = true
  tags                      = module.context.common_tag_list

  private_network {
    pn_id       = scaleway_vpc_private_network.main.id
    enable_ipam = true
  }
}

# The application database is not a Terraform resource: Scaleway creates every
# database owned by its managed superadmin role, and the migration owner role
# must own the application database. `kodama migrate` creates it as the
# administrator (a maintenance connection to var.postgres_admin_database) and
# hands it to the owner role.
