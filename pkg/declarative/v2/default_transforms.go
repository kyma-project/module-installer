package v2

import (
	"context"

	"github.com/kyma-project/module-manager/pkg/types"
)

const (
	DisclaimerAnnotation      = "reconciler.kyma-project.io/managed-by-reconciler-disclaimer"
	disclaimerAnnotationValue = "DO NOT EDIT - This resource is managed by Kyma.\n" +
		"Any modifications are discarded and the resource is reverted to the original state."
)

func disclaimerTransform(_ context.Context, _ Object, resources *types.ManifestResources) error {
	for _, resource := range resources.Items {
		annotations := resource.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[DisclaimerAnnotation] = disclaimerAnnotationValue
		resource.SetAnnotations(annotations)
	}
	return nil
}

func kymaComponentTransform(_ context.Context, obj Object, resources *types.ManifestResources) error {
	for _, resource := range resources.Items {
		labels := resource.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels["app.kubernetes.io/component"] = obj.ComponentName()
		labels["app.kubernetes.io/part-of"] = "Kyma"
		resource.SetLabels(labels)
	}
	return nil
}

func managedByDeclarativeV2(_ context.Context, _ Object, resources *types.ManifestResources) error {
	managedBy := "declarative-v2"
	for _, resource := range resources.Items {
		labels := resource.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		// legacy managed by value
		labels["reconciler.kyma-project.io/managed-by"] = managedBy
		resource.SetLabels(labels)
	}
	return nil
}
