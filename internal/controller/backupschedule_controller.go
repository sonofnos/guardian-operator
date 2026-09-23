package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	guardianv1alpha1 "github.com/sonofnos/guardian-operator/api/v1alpha1"
)

const (
	backupScheduleFinalizer = "guardian.sonofnos.dev/backupschedule-finalizer"

	// backupJobImage is a placeholder container image used by the generated
	// CronJob. It does not perform a real database/volume snapshot - see the
	// README "Disaster recovery design" section for the intended extension
	// point.
	backupJobImage = "busybox:1.36"
)

// BackupScheduleReconciler reconciles a BackupSchedule object.
type BackupScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=guardian.sonofnos.dev,resources=backupschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=guardian.sonofnos.dev,resources=backupschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=guardian.sonofnos.dev,resources=backupschedules/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *BackupScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	bs := &guardianv1alpha1.BackupSchedule{}
	if err := r.Get(ctx, req.NamespacedName, bs); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching BackupSchedule: %w", err)
	}

	// Handle deletion: run finalizer cleanup, then let GC remove owned
	// objects via owner references.
	if !bs.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(bs, backupScheduleFinalizer) {
			if err := r.cleanupManagedResources(ctx, bs); err != nil {
				return ctrl.Result{}, fmt.Errorf("cleaning up managed resources: %w", err)
			}
			controllerutil.RemoveFinalizer(bs, backupScheduleFinalizer)
			if err := r.Update(ctx, bs); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(bs, backupScheduleFinalizer) {
		controllerutil.AddFinalizer(bs, backupScheduleFinalizer)
		if err := r.Update(ctx, bs); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Update triggers a new reconcile; return now to work off the fresh object.
		return ctrl.Result{}, nil
	}

	// Validate the target workload exists before wiring up a CronJob against it.
	if err := r.validateTarget(ctx, bs); err != nil {
		r.setCondition(bs, guardianv1alpha1.ConditionTypeDegraded, metav1.ConditionTrue, "TargetNotFound", err.Error())
		r.setCondition(bs, guardianv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "TargetNotFound", err.Error())
		if statusErr := r.Status().Update(ctx, bs); statusErr != nil {
			logger.Error(statusErr, "failed updating status after target validation failure")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	cronJob, err := r.reconcileCronJob(ctx, bs)
	if err != nil {
		r.setCondition(bs, guardianv1alpha1.ConditionTypeDegraded, metav1.ConditionTrue, "ReconcileError", err.Error())
		r.setCondition(bs, guardianv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, bs); statusErr != nil {
			logger.Error(statusErr, "failed updating status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	if err := r.trimRetainedJobs(ctx, bs); err != nil {
		logger.Error(err, "failed trimming old backup jobs")
		// Non-fatal: surface as Degraded but do not block Ready state on it.
		r.setCondition(bs, guardianv1alpha1.ConditionTypeDegraded, metav1.ConditionTrue, "RetentionTrimFailed", err.Error())
	} else {
		r.setCondition(bs, guardianv1alpha1.ConditionTypeDegraded, metav1.ConditionFalse, "AsExpected", "no issues observed")
	}

	bs.Status.ManagedCronJobName = cronJob.Name
	bs.Status.ObservedGeneration = bs.Generation
	if cronJob.Status.LastScheduleTime != nil {
		bs.Status.LastScheduleTime = cronJob.Status.LastScheduleTime
	}
	r.updateLastSuccessfulTime(ctx, bs)

	r.setCondition(bs, guardianv1alpha1.ConditionTypeReady, metav1.ConditionTrue, "CronJobReconciled", "backup CronJob is reconciled")
	if err := r.Status().Update(ctx, bs); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *BackupScheduleReconciler) validateTarget(ctx context.Context, bs *guardianv1alpha1.BackupSchedule) error {
	key := types.NamespacedName{Namespace: bs.Namespace, Name: bs.Spec.TargetRef.Name}
	switch bs.Spec.TargetRef.Kind {
	case "Deployment":
		d := &appsv1.Deployment{}
		if err := r.Get(ctx, key, d); err != nil {
			return fmt.Errorf("target Deployment %q not found: %w", bs.Spec.TargetRef.Name, err)
		}
		return nil
	case "StatefulSet":
		s := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, s); err != nil {
			return fmt.Errorf("target StatefulSet %q not found: %w", bs.Spec.TargetRef.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported targetRef.kind %q", bs.Spec.TargetRef.Kind)
	}
}

func cronJobName(bs *guardianv1alpha1.BackupSchedule) string {
	return fmt.Sprintf("%s-backup", bs.Name)
}

func (r *BackupScheduleReconciler) reconcileCronJob(ctx context.Context, bs *guardianv1alpha1.BackupSchedule) (*batchv1.CronJob, error) {
	desired := buildCronJob(bs)
	if err := controllerutil.SetControllerReference(bs, desired, r.Scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("creating CronJob: %w", err)
		}
		return desired, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting CronJob: %w", err)
	}

	existing.Spec = desired.Spec
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	if err := r.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("updating CronJob: %w", err)
	}
	return existing, nil
}

// buildCronJob renders the desired CronJob for a BackupSchedule. The Job
// container is a placeholder: it simulates taking a snapshot and writing a
// completion marker. It is not a real backup implementation - see README.
func buildCronJob(bs *guardianv1alpha1.BackupSchedule) *batchv1.CronJob {
	backoffLimit := int32(2)
	successHistory := int32(3)
	failedHistory := int32(1)

	script := fmt.Sprintf(
		`echo "[guardian-operator] simulating backup of %s/%s (%s) -> %s"; date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ > /tmp/backup-complete; echo "[guardian-operator] backup marker written"`,
		bs.Namespace, bs.Spec.TargetRef.Name, bs.Spec.TargetRef.Kind, bs.Spec.Destination,
	)

	podSpec := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "guardian-operator",
				"guardian.sonofnos.dev/owner":  bs.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers: []corev1.Container{
				{
					Name:    "backup",
					Image:   backupJobImage,
					Command: []string{"/bin/sh", "-c", script},
					Env: []corev1.EnvVar{
						{Name: "GUARDIAN_TARGET_KIND", Value: bs.Spec.TargetRef.Kind},
						{Name: "GUARDIAN_TARGET_NAME", Value: bs.Spec.TargetRef.Name},
						{Name: "GUARDIAN_DESTINATION", Value: bs.Spec.Destination},
					},
				},
			},
		},
	}

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName(bs),
			Namespace: bs.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "guardian-operator",
				"guardian.sonofnos.dev/owner":  bs.Name,
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   bs.Spec.Schedule,
			Suspend:                    &bs.Spec.Suspend,
			SuccessfulJobsHistoryLimit: &successHistory,
			FailedJobsHistoryLimit:     &failedHistory,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podSpec.Labels,
				},
				Spec: batchv1.JobSpec{
					BackoffLimit: &backoffLimit,
					Template:     podSpec,
				},
			},
		},
	}
	return cj
}

// trimRetainedJobs deletes the oldest backup Jobs owned by this
// BackupSchedule's CronJob beyond spec.Retention.
func (r *BackupScheduleReconciler) trimRetainedJobs(ctx context.Context, bs *guardianv1alpha1.BackupSchedule) error {
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(bs.Namespace), client.MatchingLabels{
		"guardian.sonofnos.dev/owner": bs.Name,
	}); err != nil {
		return fmt.Errorf("listing backup jobs: %w", err)
	}

	retention := int(bs.Spec.Retention)
	if retention <= 0 {
		retention = 1
	}
	if len(jobList.Items) <= retention {
		return nil
	}

	jobs := jobList.Items
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreationTimestamp.Before(&jobs[j].CreationTimestamp)
	})

	toDelete := jobs[:len(jobs)-retention]
	propagation := metav1.DeletePropagationBackground
	for i := range toDelete {
		job := toDelete[i]
		if err := r.Delete(ctx, &job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting job %s: %w", job.Name, err)
		}
	}
	return nil
}

// updateLastSuccessfulTime inspects owned Jobs for the most recent
// successful completion and records it on status.
func (r *BackupScheduleReconciler) updateLastSuccessfulTime(ctx context.Context, bs *guardianv1alpha1.BackupSchedule) {
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(bs.Namespace), client.MatchingLabels{
		"guardian.sonofnos.dev/owner": bs.Name,
	}); err != nil {
		return
	}

	var latest *metav1.Time
	for i := range jobList.Items {
		job := &jobList.Items[i]
		if job.Status.CompletionTime != nil {
			if latest == nil || job.Status.CompletionTime.After(latest.Time) {
				latest = job.Status.CompletionTime
			}
		}
	}
	if latest != nil {
		bs.Status.LastSuccessfulTime = latest
	}
}

// cleanupManagedResources runs finalizer-time cleanup. Owner references
// already cause the CronJob (and its Jobs/Pods) to be garbage collected once
// the BackupSchedule is deleted; this method exists as the explicit hook for
// any cleanup that must happen synchronously before that GC (e.g. emitting a
// final event), and to make the finalizer's purpose testable in isolation.
func (r *BackupScheduleReconciler) cleanupManagedResources(ctx context.Context, bs *guardianv1alpha1.BackupSchedule) error {
	logger := log.FromContext(ctx)
	logger.Info("running finalizer cleanup for BackupSchedule", "name", bs.Name, "namespace", bs.Namespace)

	cj := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: cronJobName(bs)}, cj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting CronJob during cleanup: %w", err)
	}
	propagation := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, cj, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting CronJob during cleanup: %w", err)
	}
	return nil
}

func (r *BackupScheduleReconciler) setCondition(bs *guardianv1alpha1.BackupSchedule, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&bs.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: bs.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&guardianv1alpha1.BackupSchedule{}).
		Owns(&batchv1.CronJob{}).
		Complete(r)
}
