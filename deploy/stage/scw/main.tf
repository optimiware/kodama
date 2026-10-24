locals {
  name_prefix         = var.installation_id
  operator_principals = [for u in var.operator_user_ids : "user_id:${u}"]
}

# --- Dedicated VPC & Private Network ---

resource "scaleway_vpc" "main" {
  name   = "${local.name_prefix}-vpc"
  region = var.region
}

resource "scaleway_vpc_private_network" "main" {
  name   = "${local.name_prefix}-pn"
  region = var.region
  vpc_id = scaleway_vpc.main.id

  ipv4_subnet {
    subnet = var.private_network_cidr
  }
}

# --- Managed Kapsule Cluster (Cilium enforces NetworkPolicy) ---

resource "scaleway_k8s_cluster" "main" {
  name                        = local.name_prefix
  region                      = var.region
  version                     = var.kubernetes_version
  cni                         = "cilium"
  private_network_id          = scaleway_vpc_private_network.main.id
  delete_additional_resources = false
}

resource "scaleway_k8s_pool" "workers" {
  cluster_id          = scaleway_k8s_cluster.main.id
  name                = "${local.name_prefix}-workers"
  node_type           = var.node_type
  size                = var.node_count
  min_size            = var.node_min_count
  max_size            = var.node_max_count
  autoscaling         = true
  autohealing         = true
  wait_for_pool_ready = true
}

# --- Scaleway Managed PostgreSQL ---

resource "random_password" "postgres_admin" {
  length           = 28
  special          = true
  min_upper        = 2
  min_lower        = 2
  min_numeric      = 2
  min_special      = 2
  override_special = "!@#%^&*-_=+"
}

resource "scaleway_rdb_instance" "postgres" {
  name                      = "${local.name_prefix}-pg"
  region                    = var.region
  node_type                 = var.db_node_type
  engine                    = var.db_engine_version
  is_ha_cluster             = var.environment_tier == "production"
  encryption_at_rest        = true
  user_name                 = "kodama_admin"
  password                  = random_password.postgres_admin.result
  disable_backup            = false
  backup_schedule_frequency = var.db_backup_frequency_hours
  backup_schedule_retention = var.db_backup_retention_days
  backup_same_region        = true

  private_network {
    pn_id       = scaleway_vpc_private_network.main.id
    enable_ipam = true
  }
}

resource "scaleway_rdb_database" "kodama" {
  instance_id = scaleway_rdb_instance.postgres.id
  name        = "kodama"
}

resource "scaleway_rdb_privilege" "admin" {
  instance_id   = scaleway_rdb_instance.postgres.id
  user_name     = scaleway_rdb_instance.postgres.user_name
  database_name = scaleway_rdb_database.kodama.name
  permission    = "all"
}

# Distinct least-privilege roles. The managed admin user cannot grant on the platform-owned database, so the
# roles and their privileges are created through the platform API (which runs as the database superadmin).

resource "random_password" "db_migration" {
  length           = 40
  special          = true
  override_special = "-_"
  min_upper        = 2
  min_lower        = 2
  min_numeric      = 2
  min_special      = 2
}

resource "random_password" "db_runtime" {
  length           = 40
  special          = true
  override_special = "-_"
  min_upper        = 2
  min_lower        = 2
  min_numeric      = 2
  min_special      = 2
}

resource "scaleway_rdb_user" "migration" {
  instance_id = scaleway_rdb_instance.postgres.id
  name        = "kodama_migration"
  password    = random_password.db_migration.result
  is_admin    = false
}

resource "scaleway_rdb_user" "runtime" {
  instance_id = scaleway_rdb_instance.postgres.id
  name        = "kodama_runtime"
  password    = random_password.db_runtime.result
  is_admin    = false
}

resource "scaleway_rdb_privilege" "migration" {
  instance_id   = scaleway_rdb_instance.postgres.id
  user_name     = scaleway_rdb_user.migration.name
  database_name = scaleway_rdb_database.kodama.name
  permission    = "all"
}

resource "scaleway_rdb_privilege" "runtime" {
  instance_id   = scaleway_rdb_instance.postgres.id
  user_name     = scaleway_rdb_user.runtime.name
  database_name = scaleway_rdb_database.kodama.name
  permission    = "readwrite"
}

# --- Scaleway Key Manager (fr-par Symmetric AES-256-GCM Keys + Scoped IAM Key) ---

resource "scaleway_key_manager_key" "kek_v1" {
  name        = "${local.name_prefix}-kek-v1"
  region      = var.region
  usage       = "symmetric_encryption"
  algorithm   = "aes_256_gcm"
  description = "Kodama scope-key KEK v1"
  unprotected = var.environment_tier != "production"
}

resource "scaleway_key_manager_key" "kek_v2" {
  name        = "${local.name_prefix}-kek-v2"
  region      = var.region
  usage       = "symmetric_encryption"
  algorithm   = "aes_256_gcm"
  description = "Kodama scope-key KEK v2 (rotation target)"
  unprotected = var.environment_tier != "production"
}

resource "scaleway_iam_application" "kodama_kms" {
  name        = "${local.name_prefix}-kms"
  description = "Least-privilege Key Manager identity for Kodama server"
}

resource "scaleway_iam_policy" "kodama_kms" {
  name           = "${local.name_prefix}-kms-policy"
  application_id = scaleway_iam_application.kodama_kms.id

  rule {
    project_ids          = [var.project_id]
    permission_set_names = ["KeyManagerReadOnly", "KeyManagerKeyEncrypt", "KeyManagerKeyDecrypt"]
  }
}

resource "scaleway_iam_api_key" "kodama_kms" {
  application_id     = scaleway_iam_application.kodama_kms.id
  default_project_id = var.project_id
  description        = "Mounted at /etc/kodama/kms-credentials/secret-key"

  lifecycle {
    ignore_changes = [expires_at]
  }
}

# --- Scaleway Object Storage (Separated: Artifacts vs Loki Logs) ---

resource "scaleway_object_bucket" "artifacts" {
  name          = "${local.name_prefix}-artifacts"
  region        = var.region
  force_destroy = var.environment_tier != "production"

  versioning {
    enabled = true
  }
}

resource "scaleway_object_bucket" "logs" {
  name          = "${local.name_prefix}-loki-logs"
  region        = var.region
  force_destroy = var.environment_tier != "production"
}

resource "scaleway_iam_application" "artifacts_s3" {
  name        = "${local.name_prefix}-artifacts-s3"
  description = "Kodama S3 object storage identity"
}

resource "scaleway_iam_policy" "artifacts_s3" {
  name           = "${local.name_prefix}-artifacts-s3"
  application_id = scaleway_iam_application.artifacts_s3.id

  rule {
    project_ids          = [var.project_id]
    permission_set_names = ["ObjectStorageBucketsRead", "ObjectStorageObjectsRead", "ObjectStorageObjectsWrite", "ObjectStorageObjectsDelete"]
  }
}

resource "scaleway_iam_api_key" "artifacts_s3" {
  application_id     = scaleway_iam_application.artifacts_s3.id
  default_project_id = var.project_id

  lifecycle {
    ignore_changes = [expires_at]
  }
}

resource "scaleway_object_bucket_policy" "artifacts" {
  bucket = scaleway_object_bucket.artifacts.name
  region = var.region
  policy = jsonencode({
    Version = "2023-04-17"
    Id      = "${local.name_prefix}-artifacts-bucket-policy"
    Statement = [
      {
        Sid       = "AllowArtifactsApplicationOnly"
        Effect    = "Allow"
        Principal = { SCW = "application_id:${scaleway_iam_application.artifacts_s3.id}" }
        Action    = ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource = [
          scaleway_object_bucket.artifacts.name,
          "${scaleway_object_bucket.artifacts.name}/*",
        ]
      },
      {
        Sid       = "AllowOperatorsFullAccess"
        Effect    = "Allow"
        Principal = { SCW = local.operator_principals }
        Action    = "s3:*"
        Resource = [
          scaleway_object_bucket.artifacts.name,
          "${scaleway_object_bucket.artifacts.name}/*",
        ]
      }
    ]
  })
}

resource "scaleway_iam_application" "logs_s3" {
  name        = "${local.name_prefix}-loki-s3"
  description = "Loki S3 log storage identity"
}

resource "scaleway_iam_policy" "logs_s3" {
  name           = "${local.name_prefix}-loki-s3"
  application_id = scaleway_iam_application.logs_s3.id

  rule {
    project_ids          = [var.project_id]
    permission_set_names = ["ObjectStorageBucketsRead", "ObjectStorageObjectsRead", "ObjectStorageObjectsWrite", "ObjectStorageObjectsDelete"]
  }
}

resource "scaleway_iam_api_key" "logs_s3" {
  application_id     = scaleway_iam_application.logs_s3.id
  default_project_id = var.project_id

  lifecycle {
    ignore_changes = [expires_at]
  }
}

resource "scaleway_object_bucket_policy" "logs" {
  bucket = scaleway_object_bucket.logs.name
  region = var.region
  policy = jsonencode({
    Version = "2023-04-17"
    Id      = "${local.name_prefix}-logs-bucket-policy"
    Statement = [
      {
        Sid       = "AllowLogsApplicationOnly"
        Effect    = "Allow"
        Principal = { SCW = "application_id:${scaleway_iam_application.logs_s3.id}" }
        Action    = ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource = [
          scaleway_object_bucket.logs.name,
          "${scaleway_object_bucket.logs.name}/*",
        ]
      },
      {
        Sid       = "AllowOperatorsFullAccess"
        Effect    = "Allow"
        Principal = { SCW = local.operator_principals }
        Action    = "s3:*"
        Resource = [
          scaleway_object_bucket.logs.name,
          "${scaleway_object_bucket.logs.name}/*",
        ]
      }
    ]
  })
}

# --- Scaleway Container Registry (public read, authenticated push; hosts the digest-pinned Kodama and workload images) ---

resource "scaleway_registry_namespace" "images" {
  name        = local.name_prefix
  region      = var.region
  description = "Digest-pinned Kodama stage images (anonymous pull, no secrets baked in)"
  is_public   = true
}
