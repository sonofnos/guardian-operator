package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	guardianv1alpha1 "github.com/sonofnos/guardian-operator/api/v1alpha1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := guardianv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newTestDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
				},
			},
		},
	}
}

func newBackupSchedule(name, namespace, targetName string) *guardianv1alpha1.BackupSchedule {
	return &guardianv1alpha1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: guardianv1alpha1.BackupScheduleSpec{
			TargetRef:   guardianv1alpha1.TargetReference{Kind: "Deployment", Name: targetName},
			Schedule:    "0 * * * *",
			Retention:   3,
			Destination: "s3://test-bucket/backups/app",
		},
	}
}

func newReconciler(t *testing.T, objs ...client.Object) (*BackupScheduleReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&guardianv1alpha1.BackupSchedule{}).
		Build()
	return &BackupScheduleReconciler{Client: cl, Scheme: scheme}, cl
}

func reconcileNTimes(t *testing.T, r *BackupScheduleReconciler, key types.NamespacedName, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile #%d failed: %v", i+1, err)
		}
	}
}

func TestReconcile_AddsFinalizerBeforeCreatingCronJob(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "myapp")
	dep := newTestDeployment("myapp", "default")
	r, cl := newReconciler(t, bs, dep)
	key := types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}

	// First reconcile: only adds the finalizer and returns early.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &guardianv1alpha1.BackupSchedule{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	found := false
	for _, f := range got.Finalizers {
		if f == backupScheduleFinalizer {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected finalizer %q to be present, got %v", backupScheduleFinalizer, got.Finalizers)
	}

	// CronJob should not exist yet after only the finalizer-adding reconcile.
	cj := &batchv1.CronJob{}
	err := cl.Get(context.Background(), types.NamespacedName{Name: cronJobName(bs), Namespace: bs.Namespace}, cj)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected CronJob to not exist yet, got err=%v", err)
	}
}

func TestReconcile_CreatesCronJobFromSpec(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "myapp")
	dep := newTestDeployment("myapp", "default")
	r, cl := newReconciler(t, bs, dep)
	key := types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}

	reconcileNTimes(t, r, key, 2) // 1: add finalizer, 2: create CronJob

	cj := &batchv1.CronJob{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: cronJobName(bs), Namespace: bs.Namespace}, cj); err != nil {
		t.Fatalf("expected CronJob to be created: %v", err)
	}

	if cj.Spec.Schedule != bs.Spec.Schedule {
		t.Errorf("schedule = %q, want %q", cj.Spec.Schedule, bs.Spec.Schedule)
	}
	if len(cj.OwnerReferences) != 1 {
		t.Fatalf("expected exactly 1 owner reference, got %d", len(cj.OwnerReferences))
	}
	if cj.OwnerReferences[0].Name != bs.Name || cj.OwnerReferences[0].Kind != "BackupSchedule" {
		t.Errorf("unexpected owner reference: %+v", cj.OwnerReferences[0])
	}
	if cj.OwnerReferences[0].Controller == nil || !*cj.OwnerReferences[0].Controller {
		t.Errorf("expected owner reference to be a controller reference")
	}

	containers := cj.Spec.JobTemplate.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0].Image != backupJobImage {
		t.Errorf("image = %q, want %q", containers[0].Image, backupJobImage)
	}

	// Status should reflect Ready=True and the managed CronJob name.
	got := &guardianv1alpha1.BackupSchedule{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ManagedCronJobName != cronJobName(bs) {
		t.Errorf("status.managedCronJobName = %q, want %q", got.Status.ManagedCronJobName, cronJobName(bs))
	}
	readyCond := findCondition(got.Status.Conditions, guardianv1alpha1.ConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True condition, got %+v", got.Status.Conditions)
	}
}

func TestReconcile_UpdatesExistingCronJobOnSpecChange(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "myapp")
	dep := newTestDeployment("myapp", "default")
	r, cl := newReconciler(t, bs, dep)
	key := types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}

	reconcileNTimes(t, r, key, 2)

	// Change the schedule and reconcile again.
	got := &guardianv1alpha1.BackupSchedule{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.Schedule = "*/15 * * * *"
	if err := cl.Update(context.Background(), got); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cj := &batchv1.CronJob{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: cronJobName(bs), Namespace: bs.Namespace}, cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Schedule != "*/15 * * * *" {
		t.Errorf("schedule not updated, got %q", cj.Spec.Schedule)
	}
}

func TestReconcile_MissingTargetSetsDegraded(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "does-not-exist")
	r, cl := newReconciler(t, bs)
	key := types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}

	reconcileNTimes(t, r, key, 2)

	got := &guardianv1alpha1.BackupSchedule{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	readyCond := findCondition(got.Status.Conditions, guardianv1alpha1.ConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %+v", got.Status.Conditions)
	}
	degradedCond := findCondition(got.Status.Conditions, guardianv1alpha1.ConditionTypeDegraded)
	if degradedCond == nil || degradedCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Degraded=True, got %+v", got.Status.Conditions)
	}

	cj := &batchv1.CronJob{}
	err := cl.Get(context.Background(), types.NamespacedName{Name: cronJobName(bs), Namespace: bs.Namespace}, cj)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no CronJob to be created for missing target, got err=%v", err)
	}
}

func TestReconcile_FinalizerCleanupDeletesCronJob(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "myapp")
	dep := newTestDeployment("myapp", "default")
	r, cl := newReconciler(t, bs, dep)
	key := types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}

	reconcileNTimes(t, r, key, 2)

	// Sanity: CronJob exists before deletion.
	cj := &batchv1.CronJob{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: cronJobName(bs), Namespace: bs.Namespace}, cj); err != nil {
		t.Fatalf("expected cronjob to exist before delete: %v", err)
	}

	// Delete the BackupSchedule (fake client honors finalizers: sets
	// DeletionTimestamp instead of removing immediately).
	got := &guardianv1alpha1.BackupSchedule{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := cl.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}

	// The BackupSchedule object should now be fully gone (finalizer removed).
	err := cl.Get(context.Background(), key, &guardianv1alpha1.BackupSchedule{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected BackupSchedule to be gone after finalizer removal, err=%v", err)
	}

	// The CronJob should have been explicitly deleted by cleanup logic.
	err = cl.Get(context.Background(), types.NamespacedName{Name: cronJobName(bs), Namespace: bs.Namespace}, &batchv1.CronJob{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected CronJob to be deleted by finalizer cleanup, err=%v", err)
	}
}

func TestTrimRetainedJobs_DeletesOldestBeyondRetention(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "myapp")
	bs.Spec.Retention = 2

	base := time.Now().Add(-1 * time.Hour)
	var objs []client.Object
	objs = append(objs, bs, newTestDeployment("myapp", "default"))

	jobNames := []string{"job-old", "job-mid", "job-new"}
	for i, name := range jobNames {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				Labels:            map[string]string{"guardian.sonofnos.dev/owner": bs.Name},
				CreationTimestamp: metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
			},
		}
		objs = append(objs, job)
	}

	r, cl := newReconciler(t, objs...)

	if err := r.trimRetainedJobs(context.Background(), bs); err != nil {
		t.Fatalf("trimRetainedJobs: %v", err)
	}

	jobList := &batchv1.JobList{}
	if err := cl.List(context.Background(), jobList, client.InNamespace("default")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobList.Items) != 2 {
		t.Fatalf("expected 2 jobs to remain, got %d", len(jobList.Items))
	}
	remaining := map[string]bool{}
	for _, j := range jobList.Items {
		remaining[j.Name] = true
	}
	if remaining["job-old"] {
		t.Errorf("expected oldest job to be deleted, but job-old still exists")
	}
	if !remaining["job-mid"] || !remaining["job-new"] {
		t.Errorf("expected job-mid and job-new to remain, got %v", remaining)
	}
}

func TestReconcile_UnsupportedTargetKindIsDegraded(t *testing.T) {
	bs := newBackupSchedule("nightly", "default", "myapp")
	bs.Spec.TargetRef.Kind = "Pod"
	r, cl := newReconciler(t, bs)
	key := types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}

	reconcileNTimes(t, r, key, 2)

	got := &guardianv1alpha1.BackupSchedule{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	readyCond := findCondition(got.Status.Conditions, guardianv1alpha1.ConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False for unsupported kind, got %+v", got.Status.Conditions)
	}
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
