package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	nativemanifests "github.com/inerplat/idleloom/deploy/native"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
)

func Apply(ctx context.Context, client dynamic.Interface, forceConflicts bool) error {
	names, err := manifestNames()
	if err != nil {
		return err
	}
	return applyNames(ctx, client, names, forceConflicts)
}

func ApplyProjection(ctx context.Context, client dynamic.Interface, forceConflicts bool) error {
	return applyNames(ctx, client, []string{"projection/rbac.yaml", "projection/admission.yaml"}, forceConflicts)
}

func applyNames(ctx context.Context, client dynamic.Interface, names []string, forceConflicts bool) error {
	for _, name := range names {
		data, err := nativemanifests.Files.ReadFile(name)
		if err != nil {
			return err
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 64<<10)
		for {
			var object unstructured.Unstructured
			if err := decoder.Decode(&object); err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("decode %s: %w", name, err)
			}
			if len(object.Object) == 0 {
				continue
			}
			gvr, namespaced, err := resourceFor(object.GetAPIVersion(), object.GetKind())
			if err != nil {
				return err
			}
			payload, err := object.MarshalJSON()
			if err != nil {
				return err
			}
			resource := client.Resource(gvr)
			options := metav1.PatchOptions{FieldManager: "idleloom"}
			if forceConflicts {
				options.Force = ptr(true)
			}
			if namespaced {
				if object.GetNamespace() == "" {
					return fmt.Errorf("%s %s has no namespace", object.GetKind(), object.GetName())
				}
				_, err = resource.Namespace(object.GetNamespace()).Patch(ctx, object.GetName(), types.ApplyPatchType, payload, options)
			} else {
				_, err = resource.Patch(ctx, object.GetName(), types.ApplyPatchType, payload, options)
			}
			if err != nil {
				return fmt.Errorf("apply %s %s: %w", object.GetKind(), object.GetName(), err)
			}
		}
	}
	return nil
}

func manifestNames() ([]string, error) {
	var names []string
	for _, directory := range []string{"crds", "rbac"} {
		entries, err := nativemanifests.Files.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := directory + "/" + entry.Name()
			if directory == "rbac" && name != "rbac/controller.yaml" && name != "rbac/operator.yaml" {
				continue
			}
			names = append(names, name)
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		leftCRD := strings.HasPrefix(names[i], "crds/")
		rightCRD := strings.HasPrefix(names[j], "crds/")
		if leftCRD != rightCRD {
			return leftCRD
		}
		return names[i] < names[j]
	})
	return names, nil
}

func resourceFor(apiVersion, kind string) (schema.GroupVersionResource, bool, error) {
	switch apiVersion + "/" + kind {
	case "apiextensions.k8s.io/v1/CustomResourceDefinition":
		return schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, false, nil
	case "v1/Namespace":
		return schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, false, nil
	case "v1/ServiceAccount":
		return schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, true, nil
	case "coordination.k8s.io/v1/Lease":
		return schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}, true, nil
	case "rbac.authorization.k8s.io/v1/ClusterRole":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, false, nil
	case "rbac.authorization.k8s.io/v1/ClusterRoleBinding":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false, nil
	case "rbac.authorization.k8s.io/v1/Role":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, true, nil
	case "rbac.authorization.k8s.io/v1/RoleBinding":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true, nil
	case "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy":
		return schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}, false, nil
	case "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding":
		return schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}, false, nil
	default:
		return schema.GroupVersionResource{}, false, fmt.Errorf("unsupported embedded manifest %s %s", apiVersion, kind)
	}
}

func ptr[T any](value T) *T { return &value }
