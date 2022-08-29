package prepare

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kyma-project/manifest-operator/operator/api/v1alpha1"
	manifestCustom "github.com/kyma-project/manifest-operator/operator/internal/pkg/custom"
	"github.com/kyma-project/manifest-operator/operator/pkg/custom"
	"github.com/kyma-project/manifest-operator/operator/pkg/descriptor"
	"github.com/kyma-project/manifest-operator/operator/pkg/labels"
	"github.com/kyma-project/manifest-operator/operator/pkg/manifest"
	"github.com/kyma-project/manifest-operator/operator/pkg/resource"
	"github.com/kyma-project/manifest-operator/operator/pkg/types"
	"helm.sh/helm/v3/pkg/strvals"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	configReadError = "reading install %s resulted in an error for " + v1alpha1.ManifestKind + " %s"
	configFileName  = "installConfig.yaml"
)

func GetInstallInfos(ctx context.Context, manifestObj *v1alpha1.Manifest, defaultClient client.Client,
	checkReadyStates bool, customStateCheck bool, codec *types.Codec,
) ([]manifest.InstallInfo, error) {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	kymaOwnerLabel, labelExists := manifestObj.Labels[labels.ComponentOwner]
	if !labelExists {
		return nil, fmt.Errorf("label %s not set for manifest resource %s",
			labels.ComponentOwner, namespacedName)
	}

	// extract config
	config := manifestObj.Spec.Config

	decodedConfig, err := descriptor.DecodeYamlFromDigest(config.Repo, config.Name, config.Ref,
		filepath.Join(config.Ref, configFileName))
	if err != nil {
		return nil, err
	}
	installConfigObj, ok := decodedConfig.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf(configReadError, ".spec.config", namespacedName)
	}
	configs, ok := installConfigObj["configs"].([]interface{})
	if !ok {
		return nil, fmt.Errorf(configReadError, "chart config object of .spec.config", namespacedName)
	}

	// evaluate rest config
	customResCheck := &manifestCustom.Resource{DefaultClient: defaultClient}

	// evaluate rest config
	clusterClient := &custom.ClusterClient{DefaultClient: defaultClient}
	restConfig, err := clusterClient.GetRestConfig(ctx, kymaOwnerLabel, manifestObj.Namespace)
	if err != nil {
		return nil, err
	}

	destinationClient, err := clusterClient.GetNewClient(restConfig, client.Options{})
	if err != nil {
		return nil, err
	}

	// check crds - if present do not update
	crds, err := parseCrds(ctx, destinationClient, &manifestObj.Spec.CRDs)
	if err != nil {
		return nil, err
	}

	manifestObjMetadata, err := runtime.DefaultUnstructuredConverter.ToUnstructured(manifestObj)
	if err != nil {
		return nil, err
	}

	// parse installs
	baseDeployInfo := manifest.InstallInfo{
		RemoteInfo: custom.RemoteInfo{
			RemoteConfig: restConfig,
			RemoteClient: &destinationClient,
		},
		ResourceInfo: manifest.ResourceInfo{
			Crds:            crds,
			BaseResource:    &unstructured.Unstructured{Object: manifestObjMetadata},
			CustomResources: []*unstructured.Unstructured{&manifestObj.Spec.Resource},
		},
		Ctx:              ctx,
		CheckFn:          customResCheck.DefaultFn,
		CheckReadyStates: checkReadyStates,
	}

	// replace with check function that checks for readiness of custom resources
	if customStateCheck {
		baseDeployInfo.CheckFn = customResCheck.CheckFn
	}

	return parseInstallations(manifestObj, codec, configs, baseDeployInfo)
}

func parseInstallations(manifestObj *v1alpha1.Manifest, codec *types.Codec,
	configs []interface{}, baseDeployInfo manifest.InstallInfo,
) ([]manifest.InstallInfo, error) {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	deployInfos := make([]manifest.InstallInfo, 0)

	for _, install := range manifestObj.Spec.Installs {
		deployInfo := baseDeployInfo

		// retrieve chart info
		chartInfo, err := getChartInfoForInstall(install, codec, manifestObj)
		if err != nil {
			return nil, err
		}

		// filter config for install
		chartConfig, chartValues, err := parseChartConfigAndValues(install, configs, namespacedName.String())
		chartValues["nameOverride"] = manifestObj.Name + "-" + install.Name
		if err != nil {
			return nil, err
		}

		// common deploy properties
		chartInfo.ReleaseName = install.Name
		chartInfo.Overrides = chartValues
		chartInfo.ClientConfig = chartConfig

		deployInfo.ChartInfo = chartInfo
		deployInfos = append(deployInfos, deployInfo)
	}

	return deployInfos, nil
}

func parseCrds(ctx context.Context, destinationClient client.Client, crdImage *types.ImageSpec,
) ([]*v1.CustomResourceDefinition, error) {
	// if crds do not exist - do nothing
	if crdImage == nil {
		return nil, nil
	}

	// extract helm chart from layer digest
	crdsPath, err := descriptor.GetPathFromExtractedTarGz(crdImage.Repo, crdImage.Name, crdImage.Ref,
		fmt.Sprintf("%s-%s", crdImage.Name, crdImage.Ref))
	if err != nil {
		return nil, err
	}

	return resource.GetCRDsFromPath(ctx, crdsPath)
}

func getChartInfoForInstall(install v1alpha1.InstallInfo, codec *types.Codec,
	manifestObj *v1alpha1.Manifest,
) (*manifest.ChartInfo, error) {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	specType, err := types.GetSpecType(install.Source.Raw)
	if err != nil {
		return nil, err
	}

	switch specType {
	case types.HelmChartType:
		var helmChartSpec types.HelmChartSpec
		if err = codec.Decode(install.Source.Raw, &helmChartSpec, specType); err != nil {
			return nil, err
		}

		return &manifest.ChartInfo{
			ChartName: fmt.Sprintf("%s/%s", install.Name, helmChartSpec.ChartName),
			RepoName:  install.Name,
			URL:       helmChartSpec.URL,
		}, nil

	case types.OciRefType:
		var imageSpec types.ImageSpec
		if err = codec.Decode(install.Source.Raw, &imageSpec, specType); err != nil {
			return nil, err
		}

		// extract helm chart from layer digest
		chartPath, err := descriptor.GetPathFromExtractedTarGz(imageSpec.Repo, imageSpec.Name, imageSpec.Ref,
			fmt.Sprintf("%s-%s", install.Name, imageSpec.Ref))
		if err != nil {
			return nil, err
		}

		return &manifest.ChartInfo{
			ChartName: install.Name,
			ChartPath: chartPath,
		}, nil
	}

	return nil, fmt.Errorf("unsupported type %s of install for Manifest %s", specType, namespacedName)
}

func getConfigAndValuesForInstall(installName string, configs []interface{}, namespacedName string) (
	string, string, error,
) {
	var defaultOverrides string
	var clientConfig string

	for _, config := range configs {
		mappedConfig, configExists := config.(map[string]interface{})
		if !configExists {
			return "", "", fmt.Errorf(configReadError, "config object", namespacedName)
		}
		if mappedConfig["name"] == installName {
			defaultOverrides, configExists = mappedConfig["overrides"].(string)
			if !configExists {
				return "", "", fmt.Errorf(configReadError, "config object overrides", namespacedName)
			}
			clientConfig, configExists = mappedConfig["clientConfig"].(string)
			if !configExists {
				return "", "", fmt.Errorf(configReadError, "chart config", namespacedName)
			}
			break
		}
	}
	return clientConfig, defaultOverrides, nil
}

func parseChartConfigAndValues(install v1alpha1.InstallInfo, configs []interface{},
	namespacedName string) (
	map[string]interface{}, map[string]interface{}, error,
) {
	configString, valuesString, err := getConfigAndValuesForInstall(install.Name, configs, namespacedName)
	if err != nil {
		return nil, nil, err
	}

	config := map[string]interface{}{}
	if err := strvals.ParseInto(configString, config); err != nil {
		return nil, nil, err
	}
	values := map[string]interface{}{}
	if err := strvals.ParseInto(valuesString, values); err != nil {
		return nil, nil, err
	}

	return config, values, nil
}
