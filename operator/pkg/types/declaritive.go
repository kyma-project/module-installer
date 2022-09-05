package types

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type CustomObject interface {
	runtime.Object
	metav1.Object
	ComponentName() string
	GetStatus() Status
	SetStatus(Status)
}

type BaseCustomObject interface {
	runtime.Object
	metav1.Object
}

// ObjectTransform is an operation that transforms the manifest objects before applying it.
type ObjectTransform = func(context.Context, BaseCustomObject, *ManifestResources) error

type State string

// Valid CustomObject States.
const (
	// StateReady signifies CustomObject is ready and has been installed successfully.
	StateReady State = "Ready"

	// StateProcessing signifies CustomObject is reconciling and is in the process of installation.
	// Processing can also signal that the Installation previously encountered an error and is now recovering.
	StateProcessing State = "Processing"

	// StateError signifies an error for CustomObject. This signifies that the Installation
	// process encountered an error.
	// Contrary to Processing, it can be expected that this state should change on the next retry.
	StateError State = "Error"

	// StateDeleting signifies CustomObject is being deleted. This is the state that is used
	// when a deletionTimestamp was detected and Finalizers are picked up.
	StateDeleting State = "Deleting"
)

// +k8s:deepcopy-gen=true

// Status defines the observed state of CustomObject.
type Status struct {
	// State signifies current state of CustomObject.
	// Value can be one of ("Ready", "Processing", "Error", "Deleting").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Processing;Deleting;Ready;Error
	State State `json:"state"`

	// Conditions associated with CustomStatus.
	Conditions []*metav1.Condition `json:"conditions,omitempty"`
}
