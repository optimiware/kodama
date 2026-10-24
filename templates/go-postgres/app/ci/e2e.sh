#!/bin/sh
# End-to-end checks against a deployment. The deployment identity (DEPLOYMENT_ID,
# CONFIGURATION_DIGEST, IMAGE_DIGEST, SOURCE_COMMIT), APP_URL and the two tenant
# IDs are explicit inputs; the test principals' tokens are delivered as secret
# files E2E_TOKEN_REVIEWER and E2E_TOKEN_OUTSIDER. The live target is verified
# to be that deployment before anything is tested.
. "$(dirname "$0")/common.sh"
build_helper
"$SCRATCH/delivery" e2e
