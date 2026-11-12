provider "aws" {
  region = var.region
}

resource "aws_s3_bucket" "tofu_state" {
  bucket        = var.bucket_name
  force_destroy = var.force_destroy

  tags = {
    app        = "kodama"
    component  = "tofu-state"
    managed-by = "opentofu"
  }
}

resource "aws_s3_bucket_versioning" "tofu_state" {
  bucket = aws_s3_bucket.tofu_state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "tofu_state" {
  bucket = aws_s3_bucket.tofu_state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "tofu_state" {
  bucket                  = aws_s3_bucket.tofu_state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

data "aws_iam_policy_document" "require_tls" {
  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    resources = [
      aws_s3_bucket.tofu_state.arn,
      "${aws_s3_bucket.tofu_state.arn}/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "require_tls" {
  bucket = aws_s3_bucket.tofu_state.id
  policy = data.aws_iam_policy_document.require_tls.json

  depends_on = [aws_s3_bucket_public_access_block.tofu_state]
}

resource "aws_s3_bucket_lifecycle_configuration" "tofu_state" {
  bucket = aws_s3_bucket.tofu_state.id

  rule {
    id     = "expire-noncurrent-state-and-lock-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_version_expiration_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}
