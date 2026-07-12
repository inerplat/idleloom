package nativemanifests

import "embed"

// Files contains the cluster-scoped Native API and controller permissions.
// Host-scoped agent permissions are created by the enrollment command.
//
//go:embed crds/*.yaml rbac/controller.yaml rbac/operator.yaml projection/rbac.yaml projection/admission.yaml
var Files embed.FS
