package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types used across guardian custom resources.
const (
	ConditionTypeReady    = "Ready"
	ConditionTypeDegraded = "Degraded"
)

// TargetReference identifies the workload a BackupSchedule protects.
type TargetReference struct {
	// Kind of the target workload. One of: Deployment, StatefulSet.
	// +kubebuilder:validation:Enum=Deployment;StatefulSet
	Kind string `json:"kind"`

	// Name of the target workload in the same namespace as the BackupSchedule.
	Name string `json:"name"`
}

// BackupScheduleSpec defines the desired state of BackupSchedule.
type BackupScheduleSpec struct {
	// TargetRef points at the Deployment or StatefulSet this schedule protects.
	TargetRef TargetReference `json:"targetRef"`

	// Schedule is a standard cron expression (e.g. "0 * * * *") controlling
	// how often a backup Job is run. This is the primary RPO knob: the
	// shorter the interval, the less data can be lost between backups.
	Schedule string `json:"schedule"`

	// Retention is the number of most recent backup Jobs to keep. Older Jobs
	// beyond this count are deleted by the controller. This is the RPO/RTO
	// depth knob: how far back in time a restore can reach.
	// +kubebuilder:validation:Minimum=1
	Retention int32 `json:"retention"`

	// Destination describes where the backup payload is written, e.g.
	// "s3://my-bucket/backups/my-app". Interpreted by the backup Job's
	// container, not by the controller itself.
	Destination string `json:"destination"`

	// Suspend pauses scheduling of new backup Jobs without deleting the
	// BackupSchedule or its history.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// BackupScheduleStatus defines the observed state of BackupSchedule.
type BackupScheduleStatus struct {
	// Conditions represent the latest available observations of the
	// BackupSchedule's state (Ready, Degraded).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastScheduleTime is the last time a backup Job was created by the
	// managed CronJob.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is the last time a backup Job completed successfully.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// ManagedCronJobName is the name of the CronJob created for this
	// BackupSchedule.
	// +optional
	ManagedCronJobName string `json:"managedCronJobName,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackupSchedule is the Schema for the backupschedules API.
type BackupSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupScheduleSpec   `json:"spec,omitempty"`
	Status BackupScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupScheduleList contains a list of BackupSchedule.
type BackupScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupSchedule{}, &BackupScheduleList{})
}
