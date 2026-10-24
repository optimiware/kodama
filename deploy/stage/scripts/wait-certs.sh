#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=deploy/stage/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# Waits until each cert-manager Certificate named by KODAMA_WAIT_CERTS ("namespace/name,...") is Ready.
# The certificates are created asynchronously by the ingress shim once an annotated Ingress exists.
# Inputs (environment): KUBECONFIG, KODAMA_WAIT_CERTS, KODAMA_WAIT_CERTS_TIMEOUT (seconds, default 900).

require_cmd kubectl
deadline=$((SECONDS + ${KODAMA_WAIT_CERTS_TIMEOUT:-900}))
IFS=',' read -r -a wanted <<<"${KODAMA_WAIT_CERTS:?KODAMA_WAIT_CERTS is required}"

for item in "${wanted[@]}"; do
  ns="${item%%/*}"
  name="${item##*/}"
  until [[ "$(kubectl -n "${ns}" get certificate "${name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" == "True" ]]; do
    if ((SECONDS >= deadline)); then
      log_error "certificate ${ns}/${name} is not Ready; recent state:"
      kubectl -n "${ns}" get certificate,certificaterequest,order,challenge 2>&1 | tail -12 >&2 || true
      die "timed out waiting for certificate ${ns}/${name} (Let's Encrypt rate limit, DNS or port 80 reachability?)"
    fi
    sleep 5
  done
  log_info "certificate ${ns}/${name} is Ready"
done
