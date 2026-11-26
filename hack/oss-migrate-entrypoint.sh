#!/usr/bin/env bash
# Entrypoint for the oss-migrate-server container.
#
# Generates an ossutil config from the OSS AK/SK env (sourced from oss-secret),
# then starts oss-migrate-server.
#
# Expected env:
#   akId, akSecret / OSS_ACCESS_KEY_ID, OSS_ACCESS_KEY_SECRET
#   OSS_ENDPOINT (e.g. oss-cn-hangzhou-internal.aliyuncs.com)
set -eu

AK_ID="${akId:-${OSS_ACCESS_KEY_ID:-}}"
AK_SECRET="${akSecret:-${OSS_ACCESS_KEY_SECRET:-}}"
ENDPOINT="${OSS_ENDPOINT:-}"

# Strip protocol from endpoint if present for ossutil config
ENDPOINT_CLEAN=$(echo "${ENDPOINT}" | sed -E 's#^https?://##')

if [ -n "${AK_ID}" ] && [ -n "${AK_SECRET}" ]; then
  if [ -z "${ENDPOINT_CLEAN}" ]; then
    echo "FATAL: OSS_ENDPOINT is empty; set it in the deployment env (e.g. oss-cn-hangzhou-internal.aliyuncs.com)" >&2
    exit 1
  fi
  cfg="${HOME:-/root}/.ossutilconfig"
  {
    echo "[Credentials]"
    echo "language=EN"
    echo "endpoint=${ENDPOINT_CLEAN}"
    echo "accessKeyID=${AK_ID}"
    echo "accessKeySecret=${AK_SECRET}"
  } > "${cfg}"
  chmod 600 "${cfg}"
  echo "ossutil config written to ${cfg} (endpoint=${ENDPOINT_CLEAN})"
else
  echo "WARN: no OSS AK/SK provided; relying on an existing ossutil config" >&2
fi

exec /usr/local/bin/oss-migrate-server "$@"
