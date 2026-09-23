package controller

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	guardianv1alpha1 "github.com/sonofnos/guardian-operator/api/v1alpha1"
)

// fakeS3 is an in-memory stand-in for the AWS S3 API used to unit-test the
// reconciler's decision logic (create-if-missing, idempotent versioning)
// without any network dependency. The kind+LocalStack e2e test
// (test/e2e/e2e_test.go) exercises the same code path against a real
// S3-compatible server to validate the wire-level behavior this fake
// approximates.
type fakeS3 struct {
	buckets           map[string]bool
	versioningStatus  map[string]s3types.BucketVersioningStatus
	headBucketErr     error
	createBucketCalls int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		buckets:          map[string]bool{},
		versioningStatus: map[string]s3types.BucketVersioningStatus{},
	}
}

type apiError struct {
	code string
}

func (e *apiError) Error() string                 { return e.code }
func (e *apiError) ErrorCode() string             { return e.code }
func (e *apiError) ErrorMessage() string          { return e.code }
func (e *apiError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }

func (f *fakeS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	name := aws.ToString(in.Bucket)
	if !f.buckets[name] {
		return nil, &apiError{code: "NotFound"}
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.createBucketCalls++
	name := aws.ToString(in.Bucket)
	if f.buckets[name] {
		return nil, &apiError{code: "BucketAlreadyOwnedByYou"}
	}
	f.buckets[name] = true
	f.versioningStatus[name] = s3types.BucketVersioningStatusSuspended
	return &s3.CreateBucketOutput{}, nil
}

func (f *fakeS3) PutBucketVersioning(_ context.Context, in *s3.PutBucketVersioningInput, _ ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error) {
	name := aws.ToString(in.Bucket)
	f.versioningStatus[name] = in.VersioningConfiguration.Status
	return &s3.PutBucketVersioningOutput{}, nil
}

func (f *fakeS3) GetBucketVersioning(_ context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	name := aws.ToString(in.Bucket)
	return &s3.GetBucketVersioningOutput{Status: f.versioningStatus[name]}, nil
}

func newS3TestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := guardianv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestEnsureBucket_CreatesWhenMissing(t *testing.T) {
	f := newFakeS3()
	if err := ensureBucket(context.Background(), f, "my-bucket", "eu-west-1"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	if !f.buckets["my-bucket"] {
		t.Fatalf("expected bucket to be created")
	}
	if f.createBucketCalls != 1 {
		t.Fatalf("expected 1 CreateBucket call, got %d", f.createBucketCalls)
	}
}

func TestEnsureBucket_IdempotentWhenExists(t *testing.T) {
	f := newFakeS3()
	f.buckets["my-bucket"] = true

	if err := ensureBucket(context.Background(), f, "my-bucket", "eu-west-1"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	if f.createBucketCalls != 0 {
		t.Fatalf("expected no CreateBucket call for existing bucket, got %d", f.createBucketCalls)
	}
}

func TestEnsureVersioning_EnablesAndIsIdempotent(t *testing.T) {
	f := newFakeS3()
	f.buckets["my-bucket"] = true
	f.versioningStatus["my-bucket"] = s3types.BucketVersioningStatusSuspended

	if err := ensureVersioning(context.Background(), f, "my-bucket", true); err != nil {
		t.Fatalf("ensureVersioning: %v", err)
	}
	if f.versioningStatus["my-bucket"] != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("expected versioning enabled, got %v", f.versioningStatus["my-bucket"])
	}

	// Calling again with the same desired state should be a no-op (idempotent).
	if err := ensureVersioning(context.Background(), f, "my-bucket", true); err != nil {
		t.Fatalf("ensureVersioning (second call): %v", err)
	}
	if f.versioningStatus["my-bucket"] != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("expected versioning to remain enabled, got %v", f.versioningStatus["my-bucket"])
	}
}

func TestEnsureVersioning_Suspends(t *testing.T) {
	f := newFakeS3()
	f.buckets["my-bucket"] = true
	f.versioningStatus["my-bucket"] = s3types.BucketVersioningStatusEnabled

	if err := ensureVersioning(context.Background(), f, "my-bucket", false); err != nil {
		t.Fatalf("ensureVersioning: %v", err)
	}
	if f.versioningStatus["my-bucket"] != s3types.BucketVersioningStatusSuspended {
		t.Fatalf("expected versioning suspended, got %v", f.versioningStatus["my-bucket"])
	}
}

func TestReconcile_S3BucketClaim_SetsReadyAndARN(t *testing.T) {
	scheme := newS3TestScheme(t)
	claim := &guardianv1alpha1.S3BucketClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim1", Namespace: "default"},
		Spec: guardianv1alpha1.S3BucketClaimSpec{
			BucketName:        "guardian-test-bucket",
			Region:            "eu-west-1",
			VersioningEnabled: true,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(&guardianv1alpha1.S3BucketClaim{}).
		Build()

	f := newFakeS3()
	r := &S3BucketClaimReconciler{
		Client: cl,
		Scheme: scheme,
		NewS3Client: func(_ context.Context, _ string) (S3API, error) {
			return f, nil
		},
	}

	key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &guardianv1alpha1.S3BucketClaim{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	readyCond := findCondition(got.Status.Conditions, guardianv1alpha1.ConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True, got %+v", got.Status.Conditions)
	}
	wantARN := "arn:aws:s3:::guardian-test-bucket"
	if got.Status.BucketARN != wantARN {
		t.Errorf("bucketArn = %q, want %q", got.Status.BucketARN, wantARN)
	}
	if !f.buckets["guardian-test-bucket"] {
		t.Fatalf("expected bucket to have been created against the fake S3 API")
	}
	if f.versioningStatus["guardian-test-bucket"] != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("expected versioning enabled on fake S3, got %v", f.versioningStatus["guardian-test-bucket"])
	}
}

func TestReconcile_S3BucketClaim_ErrorSurfacedInStatus(t *testing.T) {
	scheme := newS3TestScheme(t)
	claim := &guardianv1alpha1.S3BucketClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim1", Namespace: "default"},
		Spec: guardianv1alpha1.S3BucketClaimSpec{
			BucketName: "guardian-test-bucket",
			Region:     "eu-west-1",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(&guardianv1alpha1.S3BucketClaim{}).
		Build()

	r := &S3BucketClaimReconciler{
		Client: cl,
		Scheme: scheme,
		NewS3Client: func(_ context.Context, _ string) (S3API, error) {
			return nil, &apiError{code: "AccessDenied"}
		},
	}

	key := types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile should not return error (transient errors are requeued): %v", err)
	}

	got := &guardianv1alpha1.S3BucketClaim{}
	if err := cl.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	readyCond := findCondition(got.Status.Conditions, guardianv1alpha1.ConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %+v", got.Status.Conditions)
	}
	if readyCond.Message == "" {
		t.Errorf("expected a real error message on the condition")
	}
}
