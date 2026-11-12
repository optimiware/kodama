#!/usr/bin/env bash
# Run one real-infrastructure command inside a bounded host slot.
#
#   scripts/infra-slot.sh stack -- env KODAMA_REQUIRE_INFRA=1 go test -race -tags=integration ./test/integration/lifecycle/
#   scripts/infra-slot.sh kind  -- env KODAMA_REQUIRE_INFRA=1 go test -race -tags=integration -run TestKubernetesPipeline ./test/integration/lifecycle/
#   scripts/infra-slot.sh --exclusive <name> stack -- <command>   # one holder at a time for a suite that derives fixed names
#
# Pools: "stack" bounds real Podman/PostgreSQL/NATS runs, "kind" bounds rootless
# kind clusters. A kind run holds one kind slot AND one stack slot (kind first,
# then stack, so waiters cannot deadlock). Isolation between concurrent runs is
# already provided by the harness (generated kodama-t-<pid>-<random> cluster
# names, per-shard labels, Podman-allocated loopback ports); this script only
# bounds how many run at once and gives each run its own short TMPDIR.
#
# --exclusive NAME is a generic host-wide named lock that admits one holder at a
# time (waits obey KODAMA_SLOT_WAIT). No suite currently needs it: the runtime
# component suite is run-scoped; use it only for a suite that derives fixed
# host names.
#
# Environment:
#   KODAMA_STACK_SLOTS  concurrent stack runs   (default 6; use 1 on an 8-core host)
#   KODAMA_KIND_SLOTS   concurrent kind clusters (default 4; use 1 on an 8-core host)
#   KODAMA_LOAD_MAX     start no new run while the 1-minute load average exceeds
#                       this value (default: 80% of the CPU count)
#   KODAMA_SLOT_WAIT    seconds to wait for a slot before exiting 75 (default 3600)
#   KODAMA_KEYS_MIN     warn when this user's kernel key quota (kernel.keys.maxkeys)
#                       is below this value (default 2000)
#   KODAMA_SLOT_DIR     lock directory (default /tmp/oc/slots)
#   TMPDIR              honoured when set and at most 24 bytes; otherwise /tmp/oc/<pool>-<slot>
#   KODAMA_TOOLS_DIR    directory holding the pinned host tools
#                       (default $HOME/.local/bin): a kind run prepends it to PATH
#                       and, unless already set, points KODAMA_KIND_BIN at
#                       kind-0.33.0 there. The harness pins kind 0.33.0, so the
#                       directory must provide a kubectl matching node v1.37
#                       (the default install also carries kubectl v1.34.13).
#                       /tmp/oc/bin is the legacy location for lanes pinned to
#                       the previous toolchain.
#
# The slot lock is held by the child's inherited descriptor, so it is released
# when the command and everything it started exit, even if this script is killed.
set -u

exclusive=""
if [ "${1:-}" = "--exclusive" ]; then
  exclusive=${2:-}
  case "$exclusive" in
    "" | *[!A-Za-z0-9._-]*) echo "infra-slot: --exclusive needs a simple name" >&2; exit 64 ;;
  esac
  shift 2
fi
if [ $# -lt 3 ] || [ "$2" != "--" ]; then
  echo "usage: $0 [--exclusive NAME] stack|kind -- command [args...]" >&2
  exit 64
fi
pool=$1
shift 2
case "$pool" in
  stack | kind) ;;
  *) echo "infra-slot: unknown pool '$pool' (want stack or kind)" >&2; exit 64 ;;
esac

dir=${KODAMA_SLOT_DIR:-/tmp/oc/slots}
mkdir -p "$dir"
cpus=$(nproc 2>/dev/null || echo 8)
stack_slots=${KODAMA_STACK_SLOTS:-6}
kind_slots=${KODAMA_KIND_SLOTS:-4}
load_max=${KODAMA_LOAD_MAX:-$((cpus * 8 / 10))}
wait_max=${KODAMA_SLOT_WAIT:-3600}
started=$SECONDS

acquire() { # pool count fdvar -> sets the descriptor and the chosen index
  local name=$1 count=$2 i fd
  while :; do
    for ((i = 1; i <= count; i++)); do
      exec {fd}>"$dir/$name.$i.lock"
      if flock -n "$fd"; then
        eval "$3=$fd"
        slot_index=$i
        return 0
      fi
      exec {fd}>&-
    done
    if ((SECONDS - started > wait_max)); then
      echo "infra-slot: no free $name slot after ${wait_max}s" >&2
      exit 75
    fi
    sleep 2
  done
}

load_ok() {
  local load
  load=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null || echo 0)
  awk -v l="$load" -v m="$load_max" 'BEGIN { exit !(l <= m) }'
}

inherited_lock() { # fd target_file -> 0 when this process already holds fd on target_file
  local fd=${1:-} target=${2:-} fd_path target_path
  case "$fd" in
    "" | *[!0-9]*) return 1 ;;
  esac
  [ -e "/proc/$$/fd/$fd" ] && [ -e "$target" ] || return 1
  fd_path=$(readlink -f "/proc/$$/fd/$fd" 2>/dev/null || true)
  target_path=$(readlink -f "$target" 2>/dev/null || true)
  [ -n "$fd_path" ] && [ "$fd_path" = "$target_path" ]
}

if [ -n "$exclusive" ]; then
  if [ "${KODAMA_EXCLUSIVE_NAME:-}" = "$exclusive" ] &&
     inherited_lock "${KODAMA_EXCLUSIVE_FD:-}" "$dir/excl.$exclusive.lock"; then
    excl_fd=$KODAMA_EXCLUSIVE_FD
  else
    exec {excl_fd}>"$dir/excl.$exclusive.lock"
    until flock -n "$excl_fd"; do
      if ((SECONDS - started > wait_max)); then
        echo "infra-slot: exclusive '$exclusive' still held after ${wait_max}s" >&2
        exit 75
      fi
      sleep 2
    done
    export KODAMA_EXCLUSIVE_NAME="$exclusive"
    export KODAMA_EXCLUSIVE_FD="$excl_fd"
  fi
fi

kind_fd=""
if [ "$pool" = "kind" ]; then
  if [ -n "${KODAMA_KIND_INDEX:-}" ] &&
     inherited_lock "${KODAMA_KIND_FD:-}" "$dir/kind.${KODAMA_KIND_INDEX}.lock"; then
    kind_fd=$KODAMA_KIND_FD
    kind_index=$KODAMA_KIND_INDEX
  else
    acquire kind "$kind_slots" kind_fd
    kind_index=$slot_index
    export KODAMA_KIND_FD="$kind_fd"
    export KODAMA_KIND_INDEX="$kind_index"
  fi
fi
stack_fd=""
if [ -n "${KODAMA_STACK_INDEX:-}" ] &&
   inherited_lock "${KODAMA_STACK_FD:-}" "$dir/stack.${KODAMA_STACK_INDEX}.lock"; then
  stack_fd=$KODAMA_STACK_FD
  stack_index=$KODAMA_STACK_INDEX
else
  acquire stack "$stack_slots" stack_fd
  stack_index=$slot_index
  export KODAMA_STACK_FD="$stack_fd"
  export KODAMA_STACK_INDEX="$stack_index"
fi

while ! load_ok; do
  if ((SECONDS - started > wait_max)); then
    echo "infra-slot: load stayed above $load_max for ${wait_max}s" >&2
    exit 75
  fi
  sleep 5
done

label="stack-$stack_index"
if [ "$pool" = "kind" ]; then
  label="kind-$kind_index"
  export KIND_EXPERIMENTAL_PROVIDER=${KIND_EXPERIMENTAL_PROVIDER:-podman}
  tools=${KODAMA_TOOLS_DIR:-$HOME/.local/bin}
  if [ -d "$tools" ] && [ -O "$tools" ]; then
    export PATH="$tools:$PATH"
    if [ -z "${KODAMA_KIND_BIN:-}" ] && [ -x "$tools/kind-0.33.0" ]; then
      export KODAMA_KIND_BIN="$tools/kind-0.33.0"
    fi
  fi
fi
export KODAMA_SLOT="$label"
# Podman and hook sockets live under TMPDIR and a Unix socket path is limited
# to 107 bytes, so a long preset TMPDIR produces "bind: invalid argument"
# failures that name no cause. Replace any TMPDIR longer than 24 bytes.
if [ -z "${TMPDIR:-}" ] || [ "$TMPDIR" = /tmp ]; then
  export TMPDIR="/tmp/oc/$label"
elif [ "${#TMPDIR}" -gt 24 ]; then
  echo "infra-slot: TMPDIR '$TMPDIR' is longer than 24 bytes; using /tmp/oc/$label (socket paths)" >&2
  export TMPDIR="/tmp/oc/$label"
fi
mkdir -p "$TMPDIR"
# Every container takes one kernel session keyring charged to the user that
# runs it, and the containers nested in a rootless kind node are charged to the
# same host user. At the quota the OCI runtime refuses to start a container
# ("create keyring: Disk quota exceeded"); the refused workload stays created
# and its test waits out a timeout that names no cause. Name it here instead.
key_usage=$(awk -v u="$(id -u):" '$1 == u { print $4 }' /proc/key-users 2>/dev/null)
key_max=${key_usage#*/}
case "$key_max" in
  "" | *[!0-9]*) key_max=$(cat /proc/sys/kernel/keys/maxkeys 2>/dev/null || echo "") ;;
esac
key_min=${KODAMA_KEYS_MIN:-2000}
case "$key_max" in
  "" | *[!0-9]*) ;;
  *)
    if [ "$key_max" -lt "$key_min" ]; then
      echo "infra-slot: this user may hold $key_max kernel keys (${key_usage:-0/$key_max} in use), below $key_min; parallel lanes can exhaust that and leave workloads unstarted. Raise it: sudo sysctl -w kernel.keys.maxkeys=$key_min kernel.keys.maxbytes=$((key_min * 100))" >&2
    fi
    ;;
esac
echo "infra-slot: running in slot $label (load $(cut -d' ' -f1 /proc/loadavg), waited $((SECONDS - started))s, TMPDIR=$TMPDIR)" >&2

exec "$@"
