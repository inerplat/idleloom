package kube

import (
	"fmt"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	WorkloadsGVR   = schema.GroupVersionResource{Group: nativev1alpha1.GroupVersion.Group, Version: nativev1alpha1.GroupVersion.Version, Resource: "idleloomworkloads"}
	ModelsGVR      = schema.GroupVersionResource{Group: nativev1alpha1.GroupVersion.Group, Version: nativev1alpha1.GroupVersion.Version, Resource: "idleloommodels"}
	HostsGVR       = schema.GroupVersionResource{Group: nativev1alpha1.GroupVersion.Group, Version: nativev1alpha1.GroupVersion.Version, Resource: "idleloomhosts"}
	AssignmentsGVR = schema.GroupVersionResource{Group: nativev1alpha1.GroupVersion.Group, Version: nativev1alpha1.GroupVersion.Version, Resource: "idleloomworkloadassignments"}
)

func FromUnstructured(source *unstructured.Unstructured, target any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(source.Object, target); err != nil {
		return fmt.Errorf("decode %s %s/%s: %w", source.GetKind(), source.GetNamespace(), source.GetName(), err)
	}
	return nil
}

func ToUnstructured(source any) (*unstructured.Unstructured, error) {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(source)
	if err != nil {
		return nil, fmt.Errorf("encode native resource: %w", err)
	}
	return &unstructured.Unstructured{Object: object}, nil
}
