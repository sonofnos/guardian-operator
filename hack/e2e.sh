#!/usr/bin/env bash
# End-to-end test: creates a kind cluster, starts a LocalStack container on
# the kind docker network, builds and loads the operator image, deploys it,
# applies sample BackupSchedule/S3BucketClaim CRs, and polls until both
# report Ready. Tears everything down on exit (success or failure).
#
# Usage: hack/e2e.sh
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-guardian-e2e}"
IMAGE="guardian-operator:e2e"
NAMESPACE="guardian-operator-system"
LOCALSTACK_NAME="guardian-e2e-localstack"
TIMEOUT_SECS="${TIMEOUT_SECS:-180}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

log() { echo "[e2e] $*" >&2; }

cleanup() {
  log "tearing down"
  docker rm -f "$LOCALSTACK_NAME" >/dev/null 2>&1 || true
  kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "creating kind cluster ${CLUSTER_NAME}"
kind create cluster --name "$CLUSTER_NAME"

log "starting LocalStack on the kind docker network"
docker rm -f "$LOCALSTACK_NAME" >/dev/null 2>&1 || true
docker run -d --name "$LOCALSTACK_NAME" \
  --network "kind" \
  -e SERVICES=s3 \
  -e DEFAULT_REGION=us-east-1 \
  localstack/localstack:3.8 >/dev/null

log "waiting for LocalStack to become healthy"
for i in $(seq 1 60); do
  if docker exec "$LOCALSTACK_NAME" curl -sf http://localhost:4566/_localstack/health >/dev/null 2>&1; then
    break
  fi
  sleep 2
  if [ "$i" -eq 60 ]; then
    log "LocalStack did not become healthy in time"
    docker logs "$LOCALSTACK_NAME" || true
    exit 1
  fi
done

log "building operator image ${IMAGE}"
docker build -t "$IMAGE" .

log "loading image into kind cluster"
kind load docker-image "$IMAGE" --name "$CLUSTER_NAME"

log "applying CRDs"
kubectl apply -f config/crd/bases/

log "applying RBAC"
kubectl apply -f config/rbac/service_account.yaml
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/rbac/role_binding.yaml
kubectl apply -f config/rbac/leader_election_role.yaml

log "deploying operator (2 replicas, leader election enabled)"
kubectl apply -f config/manager/manager.yaml
kubectl -n "$NAMESPACE" set image deployment/guardian-operator-controller-manager manager="$IMAGE"
kubectl -n "$NAMESPACE" set env deployment/guardian-operator-controller-manager \
  AWS_ENDPOINT_URL="http://${LOCALSTACK_NAME}:4566" \
  AWS_ACCESS_KEY_ID=test \
  AWS_SECRET_ACCESS_KEY=test \
  AWS_REGION=us-east-1

log "waiting for operator rollout"
kubectl -n "$NAMESPACE" rollout status deployment/guardian-operator-controller-manager --timeout="${TIMEOUT_SECS}s"

log "applying sample workload and custom resources"
kubectl apply -f config/samples/sample-app-deployment.yaml
kubectl apply -f config/samples/guardian_v1alpha1_backupschedule.yaml
kubectl apply -f config/samples/guardian_v1alpha1_s3bucketclaim.yaml

log "polling for BackupSchedule/S3BucketClaim Ready=True (timeout ${TIMEOUT_SECS}s)"
deadline=$((SECONDS + TIMEOUT_SECS))
bs_ready=""
s3_ready=""
while [ "$SECONDS" -lt "$deadline" ]; do
  bs_ready=$(kubectl get backupschedule sample-nightly-backup -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  s3_ready=$(kubectl get s3bucketclaim sample-bucket -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [ "$bs_ready" = "True" ] && [ "$s3_ready" = "True" ]; then
    break
  fi
  sleep 3
done

log "BackupSchedule Ready=${bs_ready:-<unset>}"
log "S3BucketClaim Ready=${s3_ready:-<unset>}"

if [ "$bs_ready" != "True" ] || [ "$s3_ready" != "True" ]; then
  log "one or more resources did not become Ready in time; dumping diagnostics"
  kubectl get backupschedule sample-nightly-backup -o yaml || true
  kubectl get s3bucketclaim sample-bucket -o yaml || true
  kubectl -n "$NAMESPACE" logs deployment/guardian-operator-controller-manager --all-containers --tail=200 || true
  exit 1
fi

log "verifying the managed CronJob was created"
kubectl get cronjob sample-nightly-backup-backup -o wide

log "verifying the bucket exists in LocalStack"
docker exec "$LOCALSTACK_NAME" awslocal s3api head-bucket --bucket guardian-demo-bucket

log "E2E PASSED"
