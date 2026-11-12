# Remote state is stored in Scaleway Object Storage with native S3 conditional-write
# locking (`use_lockfile = true`). `scripts/deploy` supplies the per-cluster key:
#   kodama/scw/<project_id>/<region>/<cluster_name>/infrastructure.tfstate
terraform {
  backend "s3" {
    skip_credentials_validation = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    use_path_style              = true
    use_lockfile                = true
  }
}
