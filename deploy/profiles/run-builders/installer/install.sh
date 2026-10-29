#!/bin/sh
# Installs the Kodama rootless-build node profiles on the node this Pod runs on and keeps them
# applied. Runs as the single container of the kodama-node-profiles DaemonSet (installation
# management; never in a tenant namespace). It:
#   1. copies the seccomp profile into the kubelet seccomp root atomically, and
#   2. loads the AppArmor profile with the HOST's apparmor_parser by entering the host mount
#      namespace (nsenter -t 1 -m), feeding the profile on stdin. The profile is never
#      written to the host file system and the host's /etc/apparmor.d includes resolve there.
# Then it records what it installed (status file + log) and re-checks every CHECK_INTERVAL
# seconds, reloading the AppArmor profile if the node lost it (for example after a reboot,
# which clears loaded profiles; the DaemonSet Pod is restarted and re-applies it as well).
#
# MODE=uninstall (the separate uninstall DaemonSet) removes the seccomp file and the status file
# and unloads the AppArmor profile instead, then idles so the DaemonSet rollout reports completion.
set -eu

PROFILES_DIR=${PROFILES_DIR:-/profiles}
HOST_SECCOMP_DIR=${HOST_SECCOMP_DIR:-/host-seccomp}
STATE_DIR=${STATE_DIR:-/run/kodama-node-profiles}
CHECK_INTERVAL=${CHECK_INTERVAL:-300}
MODE=${MODE:-install}
NSENTER=${NSENTER:-nsenter}
AA_PROFILES_FILE=${AA_PROFILES_FILE:-/sys/kernel/security/apparmor/profiles}
AA_ABI_FILE=${AA_ABI_FILE:-/etc/apparmor.d/abi/4.0}
SECCOMP_FILE=kodama-rootless-build-v1.json
APPARMOR_FILE=kodama-rootless-build-v1
APPARMOR_NAME=kodama-rootless-build-v1

log() { printf '%s kodama-node-profiles: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
fail() { log "ERROR: $*"; exit 1; }

host() { "$NSENTER" -t 1 -m -- "$@"; }

install_seccomp() {
  src=$PROFILES_DIR/$SECCOMP_FILE
  dst=$HOST_SECCOMP_DIR/$SECCOMP_FILE
  [ -s "$src" ] || fail "missing $src"
  if [ -f "$dst" ] && cmp -s "$src" "$dst"; then
    return 0
  fi
  tmp=$HOST_SECCOMP_DIR/.$SECCOMP_FILE.$$
  cp "$src" "$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$dst"
  log "installed seccomp profile kodama/$SECCOMP_FILE sha256=$(sha256sum "$dst" | cut -d' ' -f1)"
}

# apparmor_state prints the kernel's state of the profile: "enforce" (exactly the enforce-mode
# entry), "other" (loaded in complain or any other mode) or "absent". It returns nonzero, printing
# nothing, when the kernel's profile list cannot be read, so a failed inspection is never mistaken
# for absence.
apparmor_state() {
  list=$(host cat "$AA_PROFILES_FILE" 2>/dev/null) || return 1
  if printf '%s\n' "$list" | grep -qx "$APPARMOR_NAME (enforce)"; then
    echo enforce
  elif printf '%s\n' "$list" | grep -q "^$APPARMOR_NAME "; then
    echo other
  else
    echo absent
  fi
}

load_apparmor() {
  src=$PROFILES_DIR/$APPARMOR_FILE
  [ -s "$src" ] || fail "missing $src"
  host sh -c 'command -v apparmor_parser >/dev/null' || fail "apparmor_parser not found on the host"
  host test -e "$AA_ABI_FILE" || fail "host has no $AA_ABI_FILE (AppArmor 4.x required)"
  # -r replace or load, -K skip the cache (same flags containerd uses for its default profile).
  if ! host apparmor_parser -r -K <"$src"; then
    fail "apparmor_parser could not load $APPARMOR_NAME"
  fi
  state=$(apparmor_state) || fail "cannot inspect the kernel profile list after loading $APPARMOR_NAME"
  [ "$state" = enforce ] || fail "$APPARMOR_NAME is not loaded in enforce mode after loading (state: $state)"
  log "loaded apparmor profile $APPARMOR_NAME sha256=$(sha256sum "$src" | cut -d' ' -f1)"
}

write_status() {
  {
    printf 'profile=%s\n' "$APPARMOR_NAME"
    printf 'seccomp=kodama/%s sha256=%s\n' "$SECCOMP_FILE" "$(sha256sum "$HOST_SECCOMP_DIR/$SECCOMP_FILE" | cut -d' ' -f1)"
    printf 'apparmor=%s sha256=%s\n' "$APPARMOR_NAME" "$(sha256sum "$PROFILES_DIR/$APPARMOR_FILE" | cut -d' ' -f1)"
    printf 'applied=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$HOST_SECCOMP_DIR/.installed.$$"
  mv -f "$HOST_SECCOMP_DIR/.installed.$$" "$HOST_SECCOMP_DIR/INSTALLED"
}

apply() {
  install_seccomp
  load_apparmor
  write_status
  mkdir -p "$STATE_DIR"
  : >"$STATE_DIR/ready"
  log "profiles applied; status in /var/lib/kubelet/seccomp/kodama/INSTALLED"
}

uninstall() {
  rm -f "$HOST_SECCOMP_DIR/$SECCOMP_FILE" "$HOST_SECCOMP_DIR/INSTALLED"
  # An unreadable profile list is a failure (the Pod restarts and retries), not proof of absence.
  state=$(apparmor_state) || fail "cannot inspect the kernel profile list; removal of $APPARMOR_NAME not verified"
  if [ "$state" != absent ]; then
    host apparmor_parser -R <"$PROFILES_DIR/$APPARMOR_FILE" || fail "apparmor_parser could not remove $APPARMOR_NAME"
    state=$(apparmor_state) || fail "cannot inspect the kernel profile list; removal of $APPARMOR_NAME not verified"
    [ "$state" = absent ] || fail "$APPARMOR_NAME is still loaded (state: $state)"
  fi
  mkdir -p "$STATE_DIR"
  : >"$STATE_DIR/ready"
  log "profiles removed from this node"
}

# PID 1 ignores signals without a handler; exit promptly on Pod deletion.
trap 'log "terminating"; exit 0' TERM INT

case "$MODE" in
  install) ;;
  uninstall)
    uninstall
    [ "${RUN_ONCE:-0}" = 1 ] && exit 0
    while true; do
      sleep 3600 &
      wait $! || true
    done
    ;;
  *) fail "unknown MODE $MODE" ;;
esac

apply
# RUN_ONCE=1 is a test hook: apply once and exit instead of staying resident.
[ "${RUN_ONCE:-0}" = 1 ] && exit 0
healthy_interval=$CHECK_INTERVAL
checks=0
while true; do
  sleep "$CHECK_INTERVAL" &
  wait $! || true
  if state=$(apparmor_state); then
    if [ "$state" = enforce ]; then
      install_seccomp
      # Both profiles verified (seccomp file repaired if it drifted, AppArmor in enforce mode):
      # restore readiness that a transient inspection failure removed, and the normal interval.
      cmp -s "$PROFILES_DIR/$SECCOMP_FILE" "$HOST_SECCOMP_DIR/$SECCOMP_FILE" || fail "seccomp profile does not match after installing it"
      if [ ! -e "$STATE_DIR/ready" ]; then
        mkdir -p "$STATE_DIR"
        : >"$STATE_DIR/ready"
        log "profiles verified again; ready"
      fi
      CHECK_INTERVAL=$healthy_interval
    else
      # Missing (for example after a reboot) or not in enforce mode (complain): reload.
      log "apparmor profile $APPARMOR_NAME is $state, not enforce; reloading"
      rm -f "$STATE_DIR/ready"
      apply
    fi
  else
    # Health is unknown: report not ready and retry soon rather than assuming either way.
    log "WARNING: cannot inspect the kernel profile list; will retry"
    rm -f "$STATE_DIR/ready"
    CHECK_INTERVAL=10
  fi
  # RUN_CHECKS=N is a test hook: exit after N health checks.
  checks=$((checks + 1))
  if [ -n "${RUN_CHECKS:-}" ] && [ "$checks" -ge "$RUN_CHECKS" ]; then exit 0; fi
done
