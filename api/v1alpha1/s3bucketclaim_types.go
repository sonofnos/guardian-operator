package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// S3BucketClaimSpec defines the desired state of S3BucketClaim.
type S3BucketClaimSpec struct {
	// BucketName is the globally-unique S3 bucket name to create/manage.
	// +kubebuilder:validation:MinLength=3
	BucketName string `json:"bucketName"`

	// Region is the AWS region the bucket should live in.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// VersioningEnabled controls whether S3 object versioning is enabled on
	// the bucket.
	// +optional
	VersioningEnabled bool `json:"versioningEnabled,omitempty"`
}

// S3BucketClaimStatus defines the observed state of S3BucketClaim.
type S3BucketClaimStatus struct {
	// Conditions represent the latest available observations of the
	// S3BucketClaim's reconciliation state (Ready, Error).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// BucketARN is the ARN of the reconciled bucket, once known.
	// +optional
	BucketARN string `json:"bucketArn,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.bucketName`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// S3BucketClaim is the Schema for the s3bucketclaims API.
type S3BucketClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   S3BucketClaimSpec   `json:"spec,omitempty"`
	Status S3BucketClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// S3BucketClaimList contains a list of S3BucketClaim.
type S3BucketClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []S3BucketClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&S3BucketClaim{}, &S3BucketClaimList{})
}
