locals {
  postgres_major_version = split(".", var.postgres_engine_version)[0]
}

resource "aws_db_subnet_group" "main" {
  name       = "${module.context.name_prefix}-pg-subnets"
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_db_parameter_group" "main" {
  name   = "${module.context.name_prefix}-pg${local.postgres_major_version}"
  family = "postgres${local.postgres_major_version}"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_instance" "main" {
  identifier                 = "${module.context.name_prefix}-pg"
  engine                     = "postgres"
  engine_version             = var.postgres_engine_version
  instance_class             = var.postgres_instance_class
  allocated_storage          = var.postgres_allocated_storage_gb
  storage_type               = "gp3"
  storage_encrypted          = true
  ca_cert_identifier         = var.postgres_ca_cert_identifier
  db_name                    = "kodama"
  username                   = var.postgres_admin_user
  password_wo                = var.postgres_admin_password
  password_wo_version        = var.postgres_password_wo_version
  db_subnet_group_name       = aws_db_subnet_group.main.name
  vpc_security_group_ids     = [aws_security_group.postgres.id]
  parameter_group_name       = aws_db_parameter_group.main.name
  multi_az                   = module.context.is_ha
  publicly_accessible        = false
  backup_retention_period    = var.postgres_backup_retention_days
  backup_window              = "02:00-03:00"
  maintenance_window         = "sun:03:00-sun:04:00"
  copy_tags_to_snapshot      = true
  skip_final_snapshot        = var.postgres_skip_final_snapshot
  final_snapshot_identifier  = var.postgres_skip_final_snapshot ? null : "${module.context.name_prefix}-pg-final"
  auto_minor_version_upgrade = true
  deletion_protection        = !var.postgres_skip_final_snapshot
}
