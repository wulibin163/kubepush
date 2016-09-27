package api

import (
	"k8s.io/client-go/1.4/pkg/api/unversioned"
	"k8s.io/client-go/1.4/pkg/api/v1"
)

type PushPhase string

const (
	PushPending PushPhase = "Pending"

	PushRunning PushPhase = "Running"

	PushSucceeded PushPhase = "Succeeded"

	PushFailed PushPhase = "Failed"
)

type Push struct {
	unversioned.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// More info: http://releases.k8s.io/HEAD/docs/devel/api-conventions.md#metadata
	v1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   PushSpec   `json:"spec,omitempty"`
	Status PushStatus `json:"status,omitempty"`
}

type PushSpec struct {
	NodeName      string `json:"nodeName,omitempty"`
	PodName       string `json:"podName,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	Image         string `json:"image,omitempty"`
	//Force           bool                 `json:"force,omitempty"`
	ImagePushSecrets []v1.LocalObjectReference `json:"imagePushSecrets,omitempty"`
}

type PushStatus struct {
	Phase   PushPhase `json:"phase,omitempty"`
	Message string    `json:"message,omitempty"`
}
