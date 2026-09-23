package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	guardianv1alpha1 "github.com/sonofnos/guardian-operator/api/v1alpha1"
)

// S3ClientFactory builds an S3 API client for a given region. It is a field
// on the reconciler (rather than a package-level function) so unit tests can
// substitute a client pointed at a fake/httptest S3-compatible endpoint.
type S3ClientFactory func(ctx context.Context, region string) (S3API, error)

// S3API is the subset of the AWS S3 client used by this controller. Defining
// it locally lets tests provide a fake implementation without spinning up
// real AWS infrastructure.
type S3API interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutBucketVersioning(ctx context.Context, params *s3.PutBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error)
	GetBucketVersioning(ctx context.Context, params *s3.GetBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
}

// NewDefaultS3ClientFactory returns a factory that builds a real AWS SDK v2
// S3 client. If the AWS_ENDPOINT_URL environment variable is set, the client
// is pointed at that endpoint (e.g. LocalStack) instead of real AWS, and
// path-style addressing is used since most S3-compatible test doubles don't
// support virtual-hosted-style bucket addressing.
func NewDefaultS3ClientFactory() S3ClientFactory {
	return func(ctx context.Context, region string) (S3API, error) {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("loading AWS config: %w", err)
		}

		endpoint := os.Getenv("AWS_ENDPOINT_URL")
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
				o.UsePathStyle = true
			}
		})
		return client, nil
	}
}

// S3BucketClaimReconciler reconciles an S3BucketClaim object.
type S3BucketClaimReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	NewS3Client S3ClientFactory
}

// +kubebuilder:rbac:groups=guardian.sonofnos.dev,resources=s3bucketclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=guardian.sonofnos.dev,resources=s3bucketclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=guardian.sonofnos.dev,resources=s3bucketclaims/finalizers,verbs=update

func (r *S3BucketClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	claim := &guardianv1alpha1.S3BucketClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching S3BucketClaim: %w", err)
	}

	factory := r.NewS3Client
	if factory == nil {
		factory = NewDefaultS3ClientFactory()
	}

	s3Client, err := factory(ctx, claim.Spec.Region)
	if err != nil {
		r.setCondition(claim, guardianv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ClientError", err.Error())
		if statusErr := r.Status().Update(ctx, claim); statusErr != nil {
			logger.Error(statusErr, "failed updating status after client error")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := ensureBucket(ctx, s3Client, claim.Spec.BucketName, claim.Spec.Region); err != nil {
		r.setCondition(claim, guardianv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "BucketReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, claim); statusErr != nil {
			logger.Error(statusErr, "failed updating status after bucket error")
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := ensureVersioning(ctx, s3Client, claim.Spec.BucketName, claim.Spec.VersioningEnabled); err != nil {
		r.setCondition(claim, guardianv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "VersioningReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, claim); statusErr != nil {
			logger.Error(statusErr, "failed updating status after versioning error")
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	claim.Status.BucketARN = fmt.Sprintf("arn:aws:s3:::%s", claim.Spec.BucketName)
	claim.Status.ObservedGeneration = claim.Generation
	r.setCondition(claim, guardianv1alpha1.ConditionTypeReady, metav1.ConditionTrue, "BucketReconciled", "bucket exists and versioning matches spec")
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// ensureBucket creates the bucket if it does not already exist. It is
// idempotent: a bucket that already exists (and is owned by us) is left
// alone.
func ensureBucket(ctx context.Context, s3Client S3API, bucketName, region string) error {
	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucketName)})
	if err == nil {
		return nil // already exists
	}

	if !isNotFoundErr(err) {
		return fmt.Errorf("checking bucket existence: %w", err)
	}

	input := &s3.CreateBucketInput{Bucket: aws.String(bucketName)}
	// us-east-1 is the SDK default and must NOT be set as a
	// LocationConstraint, or AWS rejects the request.
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}

	if _, err := s3Client.CreateBucket(ctx, input); err != nil {
		if isAlreadyOwnedErr(err) {
			return nil
		}
		return fmt.Errorf("creating bucket %q: %w", bucketName, err)
	}
	return nil
}

// ensureVersioning sets the bucket's versioning configuration to match spec,
// skipping the API call if it already matches.
func ensureVersioning(ctx context.Context, s3Client S3API, bucketName string, enabled bool) error {
	current, err := s3Client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucketName)})
	if err != nil {
		return fmt.Errorf("getting bucket versioning: %w", err)
	}

	wantStatus := s3types.BucketVersioningStatusSuspended
	if enabled {
		wantStatus = s3types.BucketVersioningStatusEnabled
	}
	if current.Status == wantStatus {
		return nil
	}

	_, err = s3Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucketName),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: wantStatus,
		},
	})
	if err != nil {
		return fmt.Errorf("setting bucket versioning: %w", err)
	}
	return nil
}

func isNotFoundErr(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "404":
			return true
		}
	}
	var nf *s3types.NotFound
	return errors.As(err, &nf)
}

func isAlreadyOwnedErr(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "BucketAlreadyOwnedByYou"
	}
	return false
}

func (r *S3BucketClaimReconciler) setCondition(claim *guardianv1alpha1.S3BucketClaim, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *S3BucketClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&guardianv1alpha1.S3BucketClaim{}).
		Complete(r)
}
