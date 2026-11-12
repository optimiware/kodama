# Proves the AWS deployment root exports the operator-supplied public RDS CA
# certificate PEM bundle as `database_ca_certificate`, the exact output
# `scripts/bootstrap-secrets` reads for the `kodama-database` Secret's ca.crt.
#
# Every provider is mocked: these runs never contact AWS, TLS endpoints, a state
# backend or any account. The fixture below is a synthetic self-signed CA
# certificate used only to prove that the output is the bundle the operator
# supplied, not the RDS CA identifier and not another certificate.

mock_provider "aws" {
  mock_resource "aws_db_instance" {
    defaults = {
      id = "db-MOCK"
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{}"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/kodama-mock"
    }
  }

  mock_resource "aws_kms_key" {
    defaults = {
      arn    = "arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
      key_id = "1234abcd-12ab-34cd-56ef-1234567890ab"
    }
  }

  mock_resource "aws_eks_cluster" {
    defaults = {
      arn      = "arn:aws:eks:eu-west-1:123456789012:cluster/kodama-mock"
      endpoint = "https://mock.eks.amazonaws.com"
      id       = "kodama-mock"
      identity = [
        {
          oidc = [
            {
              issuer = "https://oidc.eks.eu-west-1.amazonaws.com/id/MOCK"
            }
          ]
        }
      ]
    }
  }

  mock_resource "aws_iam_openid_connect_provider" {
    defaults = {
      arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.eu-west-1.amazonaws.com/id/MOCK"
    }
  }
}

mock_provider "tls" {
  mock_data "tls_certificate" {
    defaults = {
      certificates = [
        {
          sha1_fingerprint = "0000000000000000000000000000000000000000"
        }
      ]
    }
  }
}

variables {
  account_id                  = "123456789012"
  k8s_api_allowed_cidrs       = ["203.0.113.7/32"]
  postgres_admin_password     = "not-a-real-password"
  postgres_ca_cert_identifier = "rds-ca-rsa2048-gss"
}

run "exports_operator_supplied_bundle" {
  command = plan

  variables {
    database_ca_bundle = <<-EOT
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
BQAwSzEqMCgGA1UEAwwhQW1hem9uIFJEUyBldS13ZXN0LTEgUm9vdCBDQSAyMDI2
MQwwCgYDVQQLDANSRFMxDzANBgNVBAoMBkFtYXpvbjAeFw0yNjExMDgxNDAxMzRa
Fw0zNjExMDUxNDAxMzRaMEsxKjAoBgNVBAMMIUFtYXpvbiBSRFMgZXUtd2VzdC0x
IFJvb3QgQ0EgMjAyNjEMMAoGA1UECwwDUkRTMQ8wDQYDVQQKDAZBbWF6b24wggEi
MA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCToL/RUjXP4iveaFJfJUiG+qEx
iMrW1eauaUd7KnqGApiFeAAoxElhdHIRiLpmOOoVVA7Yz3ktucjTNI1bmhBkMJ29
sy1zO7UWrY2q9z0Db/P8rwUIbaO3tAkHTAk7V40TRqXcPdgi7MOmZbq/y/ymG3o4
zM3tnjj+KNdamkuo6fOzib+6sMuqw4yUhLFAudxez2MSX8E1A5veR6mx5cbwSwC/
XoPNR1towGlHi9ZFq+NxkztNtMaZG+5ChNB2Wzp4nDrrY2WPAIMbMyknmYr/5x0p
ch/SVIPODelVjII/9XMogZ6G8asqA1pv60iWKjRNgfvDcyQkE/UGpib8NSBvAgMB
AAGjUzBRMB0GA1UdDgQWBBRz/rtt8tPQUvK0VEcbsULawHaJ8TAfBgNVHSMEGDAW
gBRz/rtt8tPQUvK0VEcbsULawHaJ8TAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3
DQEBCwUAA4IBAQA2u4i7NTqdqH61K0iTX8WNp8C6Eap3mD06CpWeRA1sV0FHE8c5
o+l/1deSHd3hs4TwVK+4dbjGvWITTlytqIpf+0tbd1EtOOPLlXiVMYAloa9HaDjq
qJ9CwWmS0Y+dEiiLYAnJY2dg/MtNQp24zE7MfDSqxsk5pyzr+tRdeaB7hMHpGFlN
D2y9LKely+M7mAAMX2/hwNqsgUL5xskwqrrNtunnRAH89b3vx0wl6qIQ1TccdMGh
v9JEBCapWg8BF1umrXhQaiD6jWr899R/VbZMPPkhPWDalMAz6Ov2qGwHDpMqOKN2
p5cPXUmbUgO6AGhSV/7ECjzMIfFOlqiloTFS
-----END CERTIFICATE-----
EOT
  }

  assert {
    condition     = output.database_ca_certificate == var.database_ca_bundle
    error_message = "database_ca_certificate must be the operator-supplied PEM bundle; bootstrap-secrets mounts it as ca.crt."
  }

  assert {
    condition     = output.database_ca_certificate != var.postgres_ca_cert_identifier
    error_message = "database_ca_certificate exported the RDS CA identifier; an identifier is not a PEM certificate."
  }

  assert {
    condition     = aws_db_instance.main.ca_cert_identifier == var.postgres_ca_cert_identifier
    error_message = "aws_db_instance.main.ca_cert_identifier must use the explicit RDS CA identifier."
  }
}

run "rejects_empty_bundle" {
  command = plan

  variables {
    database_ca_bundle = "  \n "
  }

  expect_failures = [var.database_ca_bundle]
}

run "rejects_identifier_as_bundle" {
  command = plan

  variables {
    database_ca_bundle = "rds-ca-rsa2048-gss"
  }

  expect_failures = [var.database_ca_bundle]
}

run "rejects_private_key_bundle" {
  command = plan

  variables {
    database_ca_bundle = <<-EOT
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----END CERTIFICATE-----
-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj
-----END PRIVATE KEY-----
EOT
  }

  expect_failures = [var.database_ca_bundle]
}

run "rejects_reversed_markers" {
  command = plan

  variables {
    database_ca_bundle = <<-EOT
-----END CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----BEGIN CERTIFICATE-----
EOT
  }

  expect_failures = [var.database_ca_bundle]
}

run "rejects_misordered_markers" {
  command = plan

  variables {
    database_ca_bundle = <<-EOT
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----END CERTIFICATE-----
-----END CERTIFICATE-----
EOT
  }

  expect_failures = [var.database_ca_bundle]
}

run "rejects_duplicate_begin_marker" {
  command = plan

  variables {
    database_ca_bundle = <<-EOT
-----BEGIN CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----END CERTIFICATE-----
EOT
  }

  expect_failures = [var.database_ca_bundle]
}

run "exports_multiple_certificates" {
  command = plan

  variables {
    database_ca_bundle = <<-EOT
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----END CERTIFICATE-----
EOT
  }

  assert {
    condition     = output.database_ca_certificate == var.database_ca_bundle
    error_message = "a multi-certificate public bundle must pass the marker rules and remain byte-identical."
  }
}

run "derives_postgres_parameter_group_and_eks_api_cidrs" {
  command = plan

  variables {
    postgres_engine_version = "17.2"
    k8s_api_allowed_cidrs   = ["198.51.100.0/24", "203.0.113.7/32"]
    database_ca_bundle      = <<-EOT
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIUQI9GeGPXLate67ba5cIaMlJOOzAwDQYJKoZIhvcNAQEL
-----END CERTIFICATE-----
EOT
  }

  assert {
    condition     = aws_db_parameter_group.main.family == "postgres17"
    error_message = "aws_db_parameter_group.main.family must derive the major version from postgres_engine_version."
  }

  assert {
    condition     = aws_db_parameter_group.main.name == "kodama-stage-pg17"
    error_message = "aws_db_parameter_group.main.name must derive the major version from postgres_engine_version."
  }

  assert {
    condition     = toset(aws_eks_cluster.main.vpc_config[0].public_access_cidrs) == toset(var.k8s_api_allowed_cidrs)
    error_message = "aws_eks_cluster.main.vpc_config[0].public_access_cidrs must match var.k8s_api_allowed_cidrs."
  }
}
