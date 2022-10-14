package declarative

import (
	"fmt"
	"github.com/go-logr/logr"
	"github.com/kyma-project/module-manager/operator/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	specKey        = "spec"
	chartPathKey   = "chartpath"
	releaseNameKey = "releasename"
	chartFlagsKey  = "chartflags"

	errMsgMandatory = "invalid type conversion for %s or does not exist in spec "
	infoMsgOptional = "invalid type conversion for %s or optional field is not given in spec"
)

// ManifestResolver represents the chart information for the passed TestCRD resource.
type DefaultManifestResolver struct{}

// Get returns the chart information to be processed.
func (m DefaultManifestResolver) Get(object types.BaseCustomObject, logger logr.Logger) (types.InstallationSpec, error) {
	objectWithSpec, valid := object.(*types.BaseCustomObjectWithSpec)
	if !valid {
		return types.InstallationSpec{},
			fmt.Errorf("invalid type conversion for `%s`", client.ObjectKeyFromObject(object))
	}
	installationSpec := objectWithSpec.Spec

	return installationSpec, nil
	//spec, ok := unstructured.Object[specKey].(map[string]interface{})
	//if !ok {
	//	return types.InstallationSpec{}, fmt.Errorf(errMsgMandatory, chartPathKey)
	//}
	//
	//// Mandatory
	//chartPath, ok := spec[chartPathKey].(string)
	//if !ok {
	//	return types.InstallationSpec{}, fmt.Errorf(errMsgMandatory, chartPathKey)
	//}
	//
	//// Optional
	//releaseName, ok := spec[releaseNameKey].(string)
	//if !ok {
	//	logger.V(2).Info(fmt.Sprintf(infoMsgOptional, releaseNameKey))
	//}
	//chartFlags, ok := spec[chartFlagsKey].(types.ChartFlags)
	//if !ok {
	//	logger.V(2).Info(fmt.Sprintf(infoMsgOptional, chartFlagsKey))
	//}

	//return types.InstallationSpec{
	//	ChartPath:   chartPath,
	//	ReleaseName: releaseName,
	//	ChartFlags:  chartFlags,
	//}, nil
}