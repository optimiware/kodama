# Remote state is stored in Amazon S3 with native conditional-write locking
# (`use_lockfile = true`). `scripts/deploy` supplies the per-cluster key:
#   kodama/aws/<account_id>/<region>/<cluster_name>/infrastructure.tfstate
terraform {
  backend "s3" {
    encrypt      = true
    use_lockfile = true
  }
}
