package util

import (
	"encoding/json"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyma-project/module-manager/operator/pkg/manifest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kyma-project/module-manager/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func getReadyConditionForComponent(manifest *v1alpha1.Manifest,
	installName string,
) (*v1alpha1.ManifestCondition, bool) {
	status := &manifest.Status
	for _, existingCondition := range status.Conditions {
		if existingCondition.Type == v1alpha1.ConditionTypeReady && existingCondition.Reason == installName {
			return &existingCondition, true
		}
	}
	return &v1alpha1.ManifestCondition{}, false
}

func AddReadyConditionForObjects(manifest *v1alpha1.Manifest, installItems []v1alpha1.InstallItem,
	conditionStatus v1alpha1.ManifestConditionStatus, message string,
) {
	status := &manifest.Status
	for _, installItem := range installItems {
		condition, exists := getReadyConditionForComponent(manifest, installItem.ChartName)
		if !exists {
			condition = &v1alpha1.ManifestCondition{
				Type:   v1alpha1.ConditionTypeReady,
				Reason: installItem.ChartName,
			}
			status.Conditions = append(status.Conditions, *condition)
		}
		condition.LastTransitionTime = &metav1.Time{Time: time.Now()}
		condition.Message = message
		condition.Status = conditionStatus
		if installItem.ClientConfig != "" || installItem.Overrides != "" {
			condition.InstallInfo = installItem
		}

		for i, existingCondition := range status.Conditions {
			if existingCondition.Type == v1alpha1.ConditionTypeReady &&
				existingCondition.Reason == installItem.ChartName {
				status.Conditions[i] = *condition
				break
			}
		}
	}
}

func AddReadyConditionForResponses(responses []*manifest.InstallResponse, logger *logr.Logger,
	manifest *v1alpha1.Manifest,
) {
	namespacedName := client.ObjectKeyFromObject(manifest)
	for _, response := range responses {
		status := v1alpha1.ConditionStatusTrue
		message := "installation successful"

		if response.Err != nil {
			status = v1alpha1.ConditionStatusFalse
			message = "installation error"
		} else if !response.Ready {
			status = v1alpha1.ConditionStatusUnknown
			message = "installation processing"
		}

		configBytes, err := json.Marshal(response.ClientConfig)
		if err != nil {
			logger.Error(err, "error marshalling chart config for",
				"resource", namespacedName)
		}

		overrideBytes, err := json.Marshal(response.Overrides)
		if err != nil {
			logger.Error(err, "error marshalling chart values for",
				"resource", namespacedName)
		}

		AddReadyConditionForObjects(manifest, []v1alpha1.InstallItem{{
			ClientConfig: string(configBytes),
			Overrides:    string(overrideBytes),
			ChartName:    response.ChartName,
		}}, status, message)
	}
}
