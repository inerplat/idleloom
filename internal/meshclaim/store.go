package meshclaim

import (
	"context"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// LeasesGVR is the claim resource. Claims are plain coordination.k8s.io Leases
// so they need no CRD and no extra RBAC beyond what leader election already
// grants.
var LeasesGVR = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

// Store is the slice of the Lease API the allocator needs.
//
// It exists because the two callers hold different clients: the worker path
// has a typed clientset and the Native path a dynamic one. Arbitration only
// works if both write the same Lease names, so they have to share the
// allocator, not just the naming convention.
type Store interface {
	// Namespace is where the claims live, for error messages.
	Namespace() string
	Get(ctx context.Context, name string) (*coordinationv1.Lease, error)
	// Create returns an apierrors.IsAlreadyExists error when the name is
	// taken. That error is the arbitration; it must not be swallowed.
	Create(ctx context.Context, lease *coordinationv1.Lease) (*coordinationv1.Lease, error)
	Delete(ctx context.Context, name string, uid types.UID) error
	List(ctx context.Context, labelSelector string) ([]coordinationv1.Lease, error)
}

// Typed adapts a clientset. An empty namespace means DefaultNamespace.
func Typed(client kubernetes.Interface, namespace string) Store {
	return typedStore{client: client, namespace: orDefault(namespace)}
}

// Dynamic adapts a dynamic client, for callers that never built a clientset.
func Dynamic(client dynamic.Interface, namespace string) Store {
	return dynamicStore{client: client, namespace: orDefault(namespace)}
}

func orDefault(namespace string) string {
	if namespace == "" {
		return DefaultNamespace
	}
	return namespace
}

type typedStore struct {
	client    kubernetes.Interface
	namespace string
}

func (s typedStore) Namespace() string { return s.namespace }

func (s typedStore) Get(ctx context.Context, name string) (*coordinationv1.Lease, error) {
	return s.client.CoordinationV1().Leases(s.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (s typedStore) Create(ctx context.Context, lease *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	return s.client.CoordinationV1().Leases(s.namespace).Create(ctx, lease, metav1.CreateOptions{})
}

func (s typedStore) Delete(ctx context.Context, name string, uid types.UID) error {
	return s.client.CoordinationV1().Leases(s.namespace).Delete(ctx, name, deleteOptions(uid))
}

func (s typedStore) List(ctx context.Context, labelSelector string) ([]coordinationv1.Lease, error) {
	list, err := s.client.CoordinationV1().Leases(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

type dynamicStore struct {
	client    dynamic.Interface
	namespace string
}

func (s dynamicStore) Namespace() string { return s.namespace }

func (s dynamicStore) resource() dynamic.ResourceInterface {
	return s.client.Resource(LeasesGVR).Namespace(s.namespace)
}

func (s dynamicStore) Get(ctx context.Context, name string) (*coordinationv1.Lease, error) {
	object, err := s.resource().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructured(object)
}

func (s dynamicStore) Create(ctx context.Context, lease *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	object, err := toUnstructured(lease)
	if err != nil {
		return nil, err
	}
	created, err := s.resource().Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructured(created)
}

func (s dynamicStore) Delete(ctx context.Context, name string, uid types.UID) error {
	return s.resource().Delete(ctx, name, deleteOptions(uid))
}

func (s dynamicStore) List(ctx context.Context, labelSelector string) ([]coordinationv1.Lease, error) {
	list, err := s.resource().List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	leases := make([]coordinationv1.Lease, 0, len(list.Items))
	for index := range list.Items {
		lease, err := fromUnstructured(&list.Items[index])
		if err != nil {
			return nil, err
		}
		leases = append(leases, *lease)
	}
	return leases, nil
}

func deleteOptions(uid types.UID) metav1.DeleteOptions {
	if uid == "" {
		return metav1.DeleteOptions{}
	}
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
}

func toUnstructured(lease *coordinationv1.Lease) (*unstructured.Unstructured, error) {
	// The dynamic client sends the object verbatim, so apiVersion and kind
	// have to be on it; a typed client fills them in from the scheme.
	copied := lease.DeepCopy()
	copied.APIVersion = "coordination.k8s.io/v1"
	copied.Kind = "Lease"
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(copied)
	if err != nil {
		return nil, fmt.Errorf("encode mesh address claim Lease/%s: %w", lease.Name, err)
	}
	return &unstructured.Unstructured{Object: fields}, nil
}

func fromUnstructured(object *unstructured.Unstructured) (*coordinationv1.Lease, error) {
	lease := &coordinationv1.Lease{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, lease); err != nil {
		return nil, fmt.Errorf("decode mesh address claim Lease/%s: %w", object.GetName(), err)
	}
	return lease, nil
}

// IsAlreadyExists is re-exported so callers do not have to import apierrors
// just to understand the outcome of a race.
func IsAlreadyExists(err error) bool { return apierrors.IsAlreadyExists(err) }
