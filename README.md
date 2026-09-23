# guardian-operator

A Kubernetes operator, hand-scaffolded against `sigs.k8s.io/controller-runtime` (the same
library the Operator SDK and Kubebuilder CLIs generate on top of), managing two custom
resources: `BackupSchedule` and `S3BucketClaim`. This is a portfolio/demonstration project,
not something running in production anywhere.

Module: `github.com/sonofnos/guardian-operator`. API group: `guardian.sonofnos.dev/v1alpha1`.

## What it does

### BackupSchedule

Declares a backup policy for a Deployment or StatefulSet in the same namespace:

```yaml
apiVersion: guardian.sonofnos.dev/v1alpha1
kind: BackupSchedule
metadata:
  name: sample-nightly-backup
spec:
  targetRef:
    kind: Deployment
    name: sample-app
  schedule: "*/2 * * * *"
  retention: 3
  destination: "s3://guardian-demo-bucket/backups/sample-app"
```

The controller (`internal/controller/backupschedule_controller.go`) reconciles this into a
`batch/v1` `CronJob` in the same namespace, named `<name>-backup`. The CronJob's Job template
runs a `busybox` container that echoes what it would back up and writes a completion marker
file. **This is a placeholder, not a real backup implementation** — see "Disaster recovery
design" below for what it's meant to be swapped for.

Reconciliation behavior:

- **Owner references**: the CronJob is created with `controllerutil.SetControllerReference`,
  so deleting the `BackupSchedule` cascades to the CronJob (and, transitively, its Jobs and
  Pods) via Kubernetes garbage collection.
- **Finalizer** (`guardian.sonofnos.dev/backupschedule-finalizer`): added on first reconcile,
  removed only after explicit cleanup runs. The cleanup step deletes the managed CronJob
  synchronously rather than relying solely on GC, and is the hook point for any future
  cleanup that needs to happen deterministically before the object disappears (e.g. writing a
  final audit event).
- **Retention trimming**: on every reconcile, the controller lists Jobs labeled with the
  owning `BackupSchedule`'s name, sorts by creation time, and deletes the oldest ones beyond
  `spec.retention`. `CronJob.spec.successfulJobsHistoryLimit`/`failedJobsHistoryLimit` also
  cap what the CronJob controller itself keeps, but retention trimming is the field the user
  actually configures and is enforced independently.
- **Target validation**: before creating the CronJob, the controller confirms
  `spec.targetRef` resolves to an existing Deployment or StatefulSet in-namespace. A missing
  target sets `Degraded=True`/`Ready=False` and requeues after 30s rather than erroring
  (errors trigger controller-runtime's exponential backoff, which is the wrong shape for "the
  user hasn't created the target yet").
- **Status**: `status.conditions` carries `Ready` and `Degraded` (`metav1.Condition`, using
  `k8s.io/apimachinery/pkg/api/meta.SetStatusCondition` so transitions are only recorded when
  something actually changed). `status.lastScheduleTime` mirrors the CronJob's own
  `status.lastScheduleTime`; `status.lastSuccessfulTime` is derived by scanning owned Jobs for
  the most recent `status.completionTime`.
- Steady-state reconciles requeue every 5 minutes to pick up drift and refresh
  `lastSuccessfulTime` even without a watch event.

### S3BucketClaim

Declares a desired S3 bucket:

```yaml
apiVersion: guardian.sonofnos.dev/v1alpha1
kind: S3BucketClaim
metadata:
  name: sample-bucket
spec:
  bucketName: guardian-demo-bucket
  region: us-east-1
  versioningEnabled: true
```

The controller (`internal/controller/s3bucketclaim_controller.go`) uses
`github.com/aws/aws-sdk-go-v2/service/s3` to:

1. `HeadBucket` — if it 404s, `CreateBucket` (idempotent: `BucketAlreadyOwnedByYou` is treated
   as success, not an error).
2. `GetBucketVersioning` / `PutBucketVersioning` — only calls `PutBucketVersioning` if the
   current status doesn't already match `spec.versioningEnabled`.

The S3 client's endpoint is controlled by the `AWS_ENDPOINT_URL` environment variable (SDK
credentials/region come from the standard AWS env vars / config chain). When set, the client
also switches to path-style addressing, since most S3-compatible test doubles (LocalStack
included) don't support virtual-hosted-style bucket addressing out of the box. Unset, it talks
to real AWS. There is no separate "local mode" flag or code path — same binary, same
reconciler, different endpoint.

`status.conditions["Ready"]` reflects actual reconciliation outcome: `True` once the bucket
exists and versioning matches spec, `False` with the real AWS/SDK error message (via
`smithy.APIError`) on failure — not a generic "error occurred" string.

The `S3API` interface in `s3bucketclaim_controller.go` is deliberately narrow (4 methods) so
unit tests can substitute an in-memory fake without a network dependency, while the e2e test
exercises the real `aws-sdk-go-v2` client against LocalStack.

## Reconciliation loop design

Both controllers follow the standard controller-runtime shape: `Reconcile` is idempotent,
side-effect-free beyond the Kubernetes/AWS API calls it explicitly makes, and returns
`ctrl.Result{RequeueAfter: ...}` rather than looping internally. Errors returned from
`Reconcile` trigger controller-runtime's built-in exponential backoff; expected/transient
conditions (missing target, AWS API error) are instead handled explicitly with a bounded
`RequeueAfter` so they don't get exponential backoff treatment they don't deserve, and so the
condition/status update always happens (returning a raw `error` skips the rest of the
function).

## High availability: leader election

`cmd/main.go` enables `manager.Options{LeaderElection: true}` by default (flag:
`--leader-elect`, on by default). This is the operator's HA/failover mechanism: run more than
one replica (see `config/manager/manager.yaml`, which sets `replicas: 2`), and
controller-runtime uses a `coordination.k8s.io/v1` `Lease` object
(`guardian-operator-leader.sonofnos.dev`) to elect a single active leader. Only the leader runs
reconciles; standbys sit idle watching the Lease. If the leader pod is killed, its node fails,
or it simply stops renewing, the Lease expires (default `LeaseDuration` 15s, `RenewDeadline`
10s, `RetryPeriod` 2s — controller-runtime defaults, left untuned here) and a standby acquires
it and takes over. This is what "the operator" being highly available actually means in
practice: not that any single reconcile is replicated, but that reconciliation resumes
automatically within roughly one lease period after a leader disappears, with no manual
intervention. RBAC for the Lease object is scoped to a `Role`/`RoleBinding` in the operator's
own namespace (`config/rbac/leader_election_role.yaml`), not cluster-wide.

## RBAC

`config/rbac/role.yaml` is generated by `controller-gen` directly from the `+kubebuilder:rbac`
markers on the reconcilers — it is not hand-maintained, and it only grants what the code
actually calls:

- `guardian.sonofnos.dev/{backupschedules,s3bucketclaims}` (+ `/status`, `/finalizers`): full
  CRUD, since these are the operator's own managed resources.
- `batch/cronjobs`: full CRUD (created, updated, owned).
- `batch/jobs`: `get;list;watch;delete` only — the operator reads and trims Jobs but never
  creates them directly (the CronJob controller does that).
- `apps/{deployments,statefulsets}`: `get;list;watch` only — read-only target validation.
- `""/events`: `create;patch` — for `record.EventRecorder` use (event emission is wired
  through the manager but currently used sparingly; the permission is scoped for it
  regardless).
- Leader election `Lease` permissions are a separate namespaced `Role`, not part of the
  cluster-scoped manager role.

No wildcard resources or verbs anywhere in the RBAC manifests.

## Running it locally (kind + LocalStack)

Prerequisites: Docker, `kind`, `kubectl`. No `operator-sdk`/`kubebuilder` CLI required.

```sh
make test-e2e
```

This runs `hack/e2e.sh`, which:

1. Creates a `kind` cluster (`guardian-e2e`).
2. Starts a `localstack/localstack` container attached to the `kind` docker network (kind
   cluster nodes and the LocalStack container resolve each other by container name over that
   network — no port-forwarding tricks needed).
3. Builds the operator image (`docker build`) and `kind load docker-image`s it into the
   cluster — no registry involved.
4. Applies the CRDs and RBAC, deploys the operator (2 replicas, leader election on), and
   points it at LocalStack via `AWS_ENDPOINT_URL`.
5. Applies a sample `Deployment`, `BackupSchedule`, and `S3BucketClaim`.
6. Polls (3s interval, 180s timeout) until both custom resources report
   `status.conditions[Ready]=True`.
7. Confirms the managed `CronJob` exists and the bucket exists in LocalStack
   (`awslocal s3api head-bucket`).
8. Tears down the LocalStack container and kind cluster unconditionally (`trap ... EXIT`).

For iterating on the controllers without the full e2e loop, `make test` runs `go vet` plus the
unit-test suite (`sigs.k8s.io/controller-runtime/pkg/client/fake`-backed, no cluster or Docker
needed) in a few seconds.

## CI

`.github/workflows/ci.yml` has two jobs:

- **unit**: `go vet`, `go build`, `go test` — runs on every push, finishes in well under a
  minute.
- **e2e**: depends on `unit`, spins up a `kind` cluster (`helm/kind-action`) and a
  `localstack/localstack` GitHub Actions service container, builds and loads the operator
  image, deploys it, applies the sample CRs, and polls for `Ready=True` — mirroring
  `hack/e2e.sh` exactly (the difference is only in how the operator reaches LocalStack: on a
  GitHub-hosted runner the service container isn't on the kind docker network, so the job
  resolves the `kind` network's bridge gateway IP and points `AWS_ENDPOINT_URL` at that
  instead of a container name).

## Disaster recovery design

The two knobs on `BackupSchedule` map directly onto the two numbers that actually matter for
DR planning:

- **`spec.schedule`** is the RPO (Recovery Point Objective) target. A `0 * * * *` schedule
  means you can lose at most ~1 hour of data between the last successful backup and a failure.
  Tighter schedule, tighter RPO, more backup Job runs.
- **`spec.retention`** is how many backup generations are kept, which combined with the
  schedule determines how far back in time a restore can reach — e.g. `schedule: "0 * * * *"`
  with `retention: 24` keeps roughly a day of hourly recovery points. It does not by itself
  determine RTO (Recovery Time Objective); how long a restore actually takes depends entirely
  on what the backup Job's payload does, which is explicitly not implemented here.

**What's real**: the CronJob lifecycle, owner references, finalizer-driven cleanup, retention
trimming against actual Job objects, and status reporting are all fully implemented and
tested against a real Kubernetes API (fake client for unit tests, a real kind cluster for
e2e).

**What's a placeholder**: the backup Job's container (`busybox`, see `buildCronJob` in
`internal/controller/backupschedule_controller.go`) only echoes what it would back up and
writes a timestamp marker to `/tmp/backup-complete`. It does not snapshot a volume, dump a
database, or write anything to `spec.destination`. The intended extension point is swapping
`backupJobImage` and the container's command for a workload-specific backup tool — e.g. a
`pg_dump`+S3-upload sidecar for Postgres, an EBS/CSI snapshot trigger for a StatefulSet's PVC,
or a `velero` invocation. `BackupSchedule` is deliberately not trying to be a general-purpose
backup product (that's what Velero, Kasten, etc. already are); it's the scheduling,
lifecycle, and status-reporting layer around a pluggable backup action, which is a realistic
scope boundary for an in-house operator that only needs to protect one or two specific
workload types.

## Repository layout

```
api/v1alpha1/            CRD Go types + generated deepcopy (controller-gen)
cmd/main.go               manager entrypoint (leader election, health probes)
internal/controller/      the two reconcilers + unit tests
config/crd/bases/         generated CRD YAML (controller-gen)
config/rbac/              generated ClusterRole + hand-written ServiceAccount/bindings
config/manager/           Deployment manifest for the operator itself
config/samples/           example CRs + a fixture Deployment for e2e/demo use
hack/e2e.sh                the kind+LocalStack e2e script (make test-e2e)
.github/workflows/ci.yml  unit job + e2e job
```

## Building blocks / non-goals

- Hand-scaffolded directly against `controller-runtime` rather than via the `operator-sdk` or
  `kubebuilder` CLIs — those CLIs are thin generators over the same library; this repo shows
  the generated shape without the generator.
- `sigs.k8s.io/controller-tools/cmd/controller-gen` (invoked via `go run`, no separate install)
  produces `zz_generated.deepcopy.go` and the CRD/RBAC YAML from `+kubebuilder:...` markers —
  nothing in `api/` or `config/` is hand-written by copying an example.
- Multi-stage `Dockerfile`: `golang:1.26` builder → `gcr.io/distroless/static-debian12:nonroot`
  final image, statically linked (`CGO_ENABLED=0`), runs as UID 65532, no shell in the final
  image.
