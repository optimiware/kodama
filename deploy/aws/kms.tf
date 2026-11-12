resource "aws_kms_key" "kodama" {
  description              = "Kodama envelope encryption key for cluster ${module.context.cluster_name}"
  key_usage                = "ENCRYPT_DECRYPT"
  customer_master_key_spec = "SYMMETRIC_DEFAULT"
  enable_key_rotation      = true
  deletion_window_in_days  = var.kms_deletion_window_days
}

resource "aws_kms_alias" "kodama" {
  name          = "alias/${module.context.name_prefix}-kek"
  target_key_id = aws_kms_key.kodama.key_id
}

locals {
  oidc_issuer_hostpath = replace(aws_iam_openid_connect_provider.eks.url, "https://", "")
}

data "aws_iam_policy_document" "kodama_irsa_assume_role" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    effect  = "Allow"

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.eks.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer_hostpath}:sub"
      values   = ["system:serviceaccount:${var.release_namespace}:kodama"]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer_hostpath}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "kodama_controller" {
  name               = "${module.context.name_prefix}-kms-controller"
  assume_role_policy = data.aws_iam_policy_document.kodama_irsa_assume_role.json
}

data "aws_iam_policy_document" "kodama_kms" {
  statement {
    sid    = "KodamaEnvelopeKMSDecrypt"
    effect = "Allow"
    actions = [
      "kms:Decrypt",
      "kms:DescribeKey",
    ]
    resources = [aws_kms_key.kodama.arn]
  }
}

resource "aws_iam_role_policy" "kodama_kms" {
  name   = "${module.context.name_prefix}-kms-policy"
  role   = aws_iam_role.kodama_controller.id
  policy = data.aws_iam_policy_document.kodama_kms.json
}
