data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  name_prefix         = var.installation_id
  azs                 = slice(data.aws_availability_zones.available.names, 0, 2)
  public_subnets      = [for idx, _ in local.azs : cidrsubnet(var.vpc_cidr, 4, idx)]
  private_subnets     = [for idx, _ in local.azs : cidrsubnet(var.vpc_cidr, 4, idx + 4)]
  effective_retention = var.environment_tier == "production" ? max(var.db_backup_retention_days, 30) : var.db_backup_retention_days
}

# --- Dedicated VPC & Subnets ---

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags                 = { Name = "${local.name_prefix}-vpc" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.name_prefix}-igw" }
}

resource "aws_subnet" "public" {
  count                   = length(local.azs)
  vpc_id                  = aws_vpc.main.id
  availability_zone       = local.azs[count.index]
  cidr_block              = local.public_subnets[count.index]
  map_public_ip_on_launch = true
  tags = {
    Name                                         = "${local.name_prefix}-public-${local.azs[count.index]}"
    "kubernetes.io/role/elb"                     = "1"
    "kubernetes.io/cluster/${local.name_prefix}" = "shared"
  }
}

resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = { Name = "${local.name_prefix}-nat-eip" }
}

resource "aws_nat_gateway" "main" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[0].id
  tags          = { Name = "${local.name_prefix}-nat" }
  depends_on    = [aws_internet_gateway.main]
}

resource "aws_subnet" "private" {
  count             = length(local.azs)
  vpc_id            = aws_vpc.main.id
  availability_zone = local.azs[count.index]
  cidr_block        = local.private_subnets[count.index]
  tags = {
    Name                                         = "${local.name_prefix}-private-${local.azs[count.index]}"
    "kubernetes.io/role/internal-elb"            = "1"
    "kubernetes.io/cluster/${local.name_prefix}" = "shared"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
}

resource "aws_route_table_association" "public" {
  count          = length(local.azs)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main.id
  }
}

resource "aws_route_table_association" "private" {
  count          = length(local.azs)
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# --- Managed EKS Cluster ---

data "aws_iam_policy_document" "eks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_cluster" {
  name               = "${local.name_prefix}-eks-cluster"
  assume_role_policy = data.aws_iam_policy_document.eks_assume.json
}

resource "aws_iam_role_policy_attachment" "eks_cluster_policy" {
  role       = aws_iam_role.eks_cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

resource "aws_eks_cluster" "main" {
  name     = local.name_prefix
  version  = var.kubernetes_version
  role_arn = aws_iam_role.eks_cluster.arn

  vpc_config {
    subnet_ids              = concat(aws_subnet.public[*].id, aws_subnet.private[*].id)
    endpoint_private_access = true
    endpoint_public_access  = true
  }

  depends_on = [aws_iam_role_policy_attachment.eks_cluster_policy]
}

data "aws_iam_policy_document" "node_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_nodes" {
  name               = "${local.name_prefix}-eks-nodes"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json
}

resource "aws_iam_role_policy_attachment" "eks_worker_node" {
  role       = aws_iam_role.eks_nodes.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
}

resource "aws_iam_role_policy_attachment" "eks_cni" {
  role       = aws_iam_role.eks_nodes.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}

resource "aws_iam_role_policy_attachment" "ecr_ro" {
  role       = aws_iam_role.eks_nodes.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
}

resource "aws_eks_node_group" "main" {
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = "${local.name_prefix}-workers"
  node_role_arn   = aws_iam_role.eks_nodes.arn
  subnet_ids      = aws_subnet.private[*].id
  instance_types  = [var.node_instance_type]

  scaling_config {
    desired_size = var.node_count
    min_size     = 1
    max_size     = max(var.node_count + 2, 4)
  }

  depends_on = [
    aws_iam_role_policy_attachment.eks_worker_node,
    aws_iam_role_policy_attachment.eks_cni,
    aws_iam_role_policy_attachment.ecr_ro,
  ]
}

data "tls_certificate" "eks_oidc" {
  url = aws_eks_cluster.main.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "eks" {
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.eks_oidc.certificates[0].sha1_fingerprint]
  url             = aws_eks_cluster.main.identity[0].oidc[0].issuer
}

# --- Cloud PostgreSQL (Amazon RDS) ---

resource "random_password" "postgres_admin" {
  length  = 32
  special = false
}

resource "aws_db_subnet_group" "postgres" {
  name       = "${local.name_prefix}-pg"
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_security_group" "postgres" {
  name        = "${local.name_prefix}-pg"
  description = "Allow PostgreSQL TLS from VPC private subnets only"
  vpc_id      = aws_vpc.main.id

  ingress {
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = local.private_subnets
  }
}

resource "aws_db_instance" "postgres" {
  identifier                  = "${local.name_prefix}-pg"
  engine                      = "postgres"
  engine_version              = "16.4"
  instance_class              = var.db_instance_class
  allocated_storage           = var.db_allocated_storage_gb
  storage_type                = "gp3"
  storage_encrypted           = true
  db_name                     = "kodama"
  username                    = "kodama_admin"
  password                    = random_password.postgres_admin.result
  db_subnet_group_name        = aws_db_subnet_group.postgres.name
  vpc_security_group_ids      = [aws_security_group.postgres.id]
  multi_az                    = var.environment_tier == "production"
  backup_retention_period     = local.effective_retention
  copy_tags_to_snapshot       = true
  deletion_protection         = var.environment_tier == "production"
  skip_final_snapshot         = var.environment_tier != "production"
  final_snapshot_identifier   = var.environment_tier == "production" ? "${local.name_prefix}-pg-final" : null
  publicly_accessible         = false
  auto_minor_version_upgrade  = true
  allow_major_version_upgrade = false
}

# --- AWS KMS (Scope-Key Wrapping, Current + Retained Rotation Key) ---

resource "aws_kms_key" "kek_v1" {
  description              = "${local.name_prefix} scope-key KEK v1"
  key_usage                = "ENCRYPT_DECRYPT"
  customer_master_key_spec = "SYMMETRIC_DEFAULT"
  enable_key_rotation      = true
  deletion_window_in_days  = 30
}

resource "aws_kms_key" "kek_v2" {
  description              = "${local.name_prefix} scope-key KEK v2 (rotation target)"
  key_usage                = "ENCRYPT_DECRYPT"
  customer_master_key_spec = "SYMMETRIC_DEFAULT"
  enable_key_rotation      = true
  deletion_window_in_days  = 30
}

data "aws_iam_policy_document" "kodama_server_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.eks.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "${replace(aws_iam_openid_connect_provider.eks.url, "https://", "")}:sub"
      values   = ["system:serviceaccount:kodama-system:kodama"]
    }
  }
}

resource "aws_iam_role" "kodama_server" {
  name               = "${local.name_prefix}-server"
  assume_role_policy = data.aws_iam_policy_document.kodama_server_assume.json
}

data "aws_iam_policy_document" "kodama_server_kms" {
  statement {
    sid       = "ScopeKeyWrapUnwrapOnly"
    actions   = ["kms:Encrypt", "kms:Decrypt"]
    resources = [aws_kms_key.kek_v1.arn, aws_kms_key.kek_v2.arn]
  }
}

resource "aws_iam_role_policy" "kodama_server_kms" {
  name   = "${local.name_prefix}-kms"
  role   = aws_iam_role.kodama_server.id
  policy = data.aws_iam_policy_document.kodama_server_kms.json
}

# --- S3 Buckets (Separated: Artifacts vs Loki Logs) ---

resource "aws_s3_bucket" "artifacts" {
  bucket        = "${local.name_prefix}-artifacts"
  force_destroy = var.environment_tier != "production"
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket" "logs" {
  bucket        = "${local.name_prefix}-loki-logs"
  force_destroy = var.environment_tier != "production"
}

resource "aws_s3_bucket_public_access_block" "logs" {
  bucket                  = aws_s3_bucket.logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Static S3 credentials required by internal/app/chartconfig.go (config.s3.credentials = static)
resource "aws_iam_user" "artifacts_s3" {
  name = "${local.name_prefix}-artifacts-s3"
}

data "aws_iam_policy_document" "artifacts_s3" {
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${aws_s3_bucket.artifacts.arn}/*"]
  }
  statement {
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.artifacts.arn]
  }
}

resource "aws_iam_user_policy" "artifacts_s3" {
  name   = "${local.name_prefix}-artifacts-s3"
  user   = aws_iam_user.artifacts_s3.name
  policy = data.aws_iam_policy_document.artifacts_s3.json
}

resource "aws_iam_access_key" "artifacts_s3" {
  user = aws_iam_user.artifacts_s3.name
}

# Dedicated S3 user for Loki log chunks (never shares Kodama artifact credentials)
resource "aws_iam_user" "logs_s3" {
  name = "${local.name_prefix}-loki-s3"
}

data "aws_iam_policy_document" "logs_s3" {
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${aws_s3_bucket.logs.arn}/*"]
  }
  statement {
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.logs.arn]
  }
}

resource "aws_iam_user_policy" "logs_s3" {
  name   = "${local.name_prefix}-loki-s3"
  user   = aws_iam_user.logs_s3.name
  policy = data.aws_iam_policy_document.logs_s3.json
}

resource "aws_iam_access_key" "logs_s3" {
  user = aws_iam_user.logs_s3.name
}
