#!/bin/sh
# Deploy a candidate image to the preview environment. Requires the run inputs
# image, image_digest, image_repository and environment_snapshot, the explicit
# runtime origin/token file, and the environment's delivered credentials.
# See docs/operations.md for the contract.
. "$(dirname "$0")/common.sh"
build_helper
"$SCRATCH/delivery" deploy --kind preview
