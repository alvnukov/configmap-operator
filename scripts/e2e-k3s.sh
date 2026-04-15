#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAMESPACE="${E2E_NAMESPACE:-e2e}"
RELEASE_NAME="${E2E_RELEASE_NAME:-configmap-operator}"
E2E_IMAGE="${E2E_IMAGE:-ghcr.io/example/configmap-operator:e2e}"
EXPECTED_CONFIG_VALUE="${E2E_EXPECTED_VALUE:-version=1}"

if [[ "${E2E_IMAGE}" =~ ^(.+):([^:/]+)$ ]]; then
  IMAGE_REF="${BASH_REMATCH[1]}"
  IMAGE_TAG="${BASH_REMATCH[2]}"
else
  echo "E2E_IMAGE must include a tag, got: ${E2E_IMAGE}" >&2
  exit 1
fi

IMAGE_REPOSITORY="${IMAGE_REF%/*}"
IMAGE_NAME="${IMAGE_REF##*/}"

debug_on_error() {
  echo
  echo "[e2e] failure diagnostics"
  kubectl -n "${NAMESPACE}" get all || true
  kubectl -n "${NAMESPACE}" get configmap e2e-config -o yaml || true
  kubectl -n "${NAMESPACE}" get deployment sample-app -o yaml | sed -n '1,240p' || true
  kubectl -n "${NAMESPACE}" get deployment "${RELEASE_NAME}" -o yaml | sed -n '1,240p' || true
  kubectl -n "${NAMESPACE}" logs "deployment/${RELEASE_NAME}" --tail=200 || true
}
trap debug_on_error ERR

echo "[e2e] applying workload fixture"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "${NAMESPACE}" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sample-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sample-app
  template:
    metadata:
      labels:
        app: sample-app
    spec:
      containers:
        - name: app
          image: busybox:1.36.1
          command: ["sh", "-c", "sleep 3600"]
EOF

kubectl -n "${NAMESPACE}" rollout status deployment/sample-app --timeout=120s

echo "[e2e] installing chart"
helm repo add helm-apps https://alvnukov.github.io/helm-apps >/dev/null
helm repo update >/dev/null
helm dependency build "${ROOT_DIR}/.helm"
helm upgrade --install "${RELEASE_NAME}" "${ROOT_DIR}/.helm" \
  --namespace "${NAMESPACE}" \
  --wait \
  --timeout 180s \
  --values "${ROOT_DIR}/test/e2e/values-k3s.yaml" \
  --set-string "apps-stateless.configmap-operator.containers.main.image.repository=${IMAGE_REPOSITORY}" \
  --set-string "apps-stateless.configmap-operator.containers.main.image.name=${IMAGE_NAME}" \
  --set-string "apps-stateless.configmap-operator.containers.main.image.staticTag=${IMAGE_TAG}"

kubectl -n "${NAMESPACE}" rollout status "deployment/${RELEASE_NAME}" --timeout=180s

echo "[e2e] waiting for ConfigMap reconciliation"
for _ in $(seq 1 40); do
  if kubectl -n "${NAMESPACE}" get configmap e2e-config >/dev/null 2>&1; then
    break
  fi
  sleep 3
done

CONFIG_VALUE="$(kubectl -n "${NAMESPACE}" get configmap e2e-config -o jsonpath='{.data.app\.conf}')"
if [[ "${CONFIG_VALUE}" != "${EXPECTED_CONFIG_VALUE}" ]]; then
  echo "expected ConfigMap data '${EXPECTED_CONFIG_VALUE}', got '${CONFIG_VALUE}'" >&2
  exit 1
fi

echo "[e2e] validating deployment patch and mounted file"
for _ in $(seq 1 40); do
  POD_NAME="$(kubectl -n "${NAMESPACE}" get pods -l app=sample-app -o jsonpath='{.items[0].metadata.name}')"
  if [[ -n "${POD_NAME}" ]]; then
    if ACTUAL_FILE_VALUE="$(kubectl -n "${NAMESPACE}" exec "${POD_NAME}" -c app -- cat /etc/config/app.conf 2>/dev/null)"; then
      if [[ "${ACTUAL_FILE_VALUE}" == "${EXPECTED_CONFIG_VALUE}" ]]; then
        break
      fi
    fi
  fi
  sleep 3
done

if [[ "${ACTUAL_FILE_VALUE:-}" != "${EXPECTED_CONFIG_VALUE}" ]]; then
  echo "expected mounted file value '${EXPECTED_CONFIG_VALUE}', got '${ACTUAL_FILE_VALUE:-<empty>}'" >&2
  exit 1
fi

echo "[e2e] success"
