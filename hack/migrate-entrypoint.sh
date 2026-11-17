#!/usr/bin/env bash
# Entrypoint for the migrate-server DaemonSet Pod.
#
# Generates an ossutil config from the OSS AK/SK env (sourced from the same
# oss-secret the cluster's OSS StorageClass uses), then starts migrate-server.
#
# Expected env (from oss-secret / DaemonSet):
#   akId, akSecret            -> AK/SK
#   OSS_ENDPOINT (optional)   -> e.g. oss-cn-hangzhou-internal.aliyuncs.com
set -eu

AK_ID="${akId:-${OSS_ACCESS_KEY_ID:-}}"
AK_SECRET="${akSecret:-${OSS_ACCESS_KEY_SECRET:-}}"
ENDPOINT="${OSS_ENDPOINT:-}"

if [ -n "${AK_ID}" ] && [ -n "${AK_SECRET}" ]; then
  # An empty endpoint would let ossutil fall back to an implicit (often
  # unreachable) default and fail later with an opaque "exit status 1" during
  # migration. Fail fast at startup instead so the misconfig is obvious.
  if [ -z "${ENDPOINT}" ]; then
    echo "FATAL: OSS_ENDPOINT is empty; set it in the DaemonSet env (e.g. oss-cn-hangzhou-internal.aliyuncs.com)" >&2
    exit 1
  fi
  cfg="${HOME:-/root}/.ossutilconfig"
  {
    echo "[Credentials]"
    echo "language=EN"
    echo "endpoint=${ENDPOINT}"
    echo "accessKeyID=${AK_ID}"
    echo "accessKeySecret=${AK_SECRET}"
  } > "${cfg}"
  chmod 600 "${cfg}"
  echo "ossutil config written to ${cfg} (endpoint=${ENDPOINT})"
else
  echo "WARN: no OSS AK/SK provided; relying on an existing ossutil config" >&2
fi

exec /usr/local/bin/migrate-server "$@"
