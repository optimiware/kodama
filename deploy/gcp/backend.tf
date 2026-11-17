# Remote state is stored in Google Cloud Storage with native GCS object locking.
# `scripts/deploy` supplies the per-cluster prefix:
#   kodema/gcp/<project_id>/<region>/<cluster_name>
terraform {
  backend "gcs" {}
}
