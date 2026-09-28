#!/bin/sh
# Crea los buckets de evidencia. Idempotente.
set -eu

mc alias set local http://minio:9000 "${MINIO_ROOT_USER}" "${MINIO_ROOT_PASSWORD}"

# Evidencia de sesión: frames seleccionados, clips, artefactos del veredicto.
mc mb --ignore-existing local/liveness-evidence

# Artefactos del banco de pruebas (material sintético o con consentimiento).
mc mb --ignore-existing local/liveness-bench

# La evidencia es dato biométrico: retención corta y explícita.
mc ilm rule add --expire-days 30 local/liveness-evidence 2>/dev/null || true

mc anonymous set none local/liveness-evidence
echo "buckets listos"
