package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"

	"helm.sh/helm/v3/pkg/strvals"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/go-containerregistry/pkg/authn"
	authnK8s "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	"github.com/kyma-project/module-manager/api/v1alpha1"
	manifestCustom "github.com/kyma-project/module-manager/internal/pkg/custom"
	internalTypes "github.com/kyma-project/module-manager/internal/pkg/types"
	"github.com/kyma-project/module-manager/pkg/custom"
	"github.com/kyma-project/module-manager/pkg/descriptor"
	"github.com/kyma-project/module-manager/pkg/labels"
	"github.com/kyma-project/module-manager/pkg/resource"
	"github.com/kyma-project/module-manager/pkg/types"
	"github.com/kyma-project/module-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const configReadError = "reading install %s resulted in an error for " + v1alpha1.ManifestKind

var ErrNoAuthSecretFound = errors.New("no auth secret found")

// GetInstallInfos pre-processes the passed Manifest CR and returns a list types.InstallInfo objects,
// each representing an installation artifact.
func GetInstallInfos(ctx context.Context, manifestObj *v1alpha1.Manifest, defaultClusterInfo types.ClusterInfo,
	flags internalTypes.ReconcileFlagConfig, processorCache types.RendererCache,
) ([]*types.InstallInfo, error) {
	// evaluate rest config
	customResCheck := &manifestCustom.Resource{DefaultClient: defaultClusterInfo.Client}

	// check crds - if present do not update
	crds, err := parseCrds(ctx, manifestObj, flags.InsecureRegistry, defaultClusterInfo.Client)
	if err != nil {
		return nil, err
	}

	manifestObjMetadata, err := runtime.DefaultUnstructuredConverter.ToUnstructured(manifestObj)
	if err != nil {
		return nil, err
	}

	// evaluate rest config
	clusterInfo, err := getDestinationConfigAndClient(ctx, defaultClusterInfo, manifestObj, processorCache,
		flags.CustomRESTCfg)
	if err != nil {
		return nil, err
	}

	// ensure runtime-watcher labels are set to CustomResource
	InsertWatcherLabels(manifestObj)

	// parse installs
	baseDeployInfo := types.InstallInfo{
		ClusterInfo: &clusterInfo,
		ResourceInfo: &types.ResourceInfo{
			Crds:            crds,
			BaseResource:    &unstructured.Unstructured{Object: manifestObjMetadata},
			CustomResources: []*unstructured.Unstructured{},
		},
		Ctx:              ctx,
		CheckFn:          customResCheck.DefaultFn,
		CheckReadyStates: flags.CheckReadyStates,
	}

	// replace with check function that checks for readiness of custom resources
	if flags.CustomStateCheck {
		baseDeployInfo.CheckFn = customResCheck.CheckFn
	}

	// add custom resource if provided
	if manifestObj.Spec.Resource.Object != nil {
		baseDeployInfo.CustomResources = append(baseDeployInfo.CustomResources, &manifestObj.Spec.Resource)
	}

	// extract config
	configs, err := parseConfigs(ctx, manifestObj.Spec.Config,
		manifestObj.Namespace, defaultClusterInfo.Client, flags.InsecureRegistry)
	if err != nil {
		return nil, err
	}
	return parseInstallations(ctx, manifestObj, flags.Codec, configs, &baseDeployInfo,
		flags.InsecureRegistry, defaultClusterInfo.Client)
}

func parseConfigs(ctx context.Context,
	config types.ImageSpec,
	namespace string,
	clusterClient client.Client,
	insecureRegistry bool,
) ([]interface{}, error) {
	var configs []any
	if config.Type.NotEmpty() { //nolint:nestif
		filePath := util.GetConfigFilePath(config)
		decodedConfig, err := getDecodedConfig(ctx, namespace, clusterClient, insecureRegistry, config, filePath)
		if err != nil {
			// if EOF error proceed without config
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, err
			}
			return configs, nil
		}
		installConfigObj, decodeOk := decodedConfig.(map[string]any)
		if !decodeOk {
			return nil, fmt.Errorf(configReadError, ".spec.config")
		}
		if installConfigObj["configs"] != nil {
			var configOk bool
			configs, configOk = installConfigObj["configs"].([]any)
			if !configOk {
				return nil, fmt.Errorf(configReadError, "chart config object of .spec.config")
			}
		}
	}
	return configs, nil
}

func getDecodedConfig(ctx context.Context,
	namespace string,
	clusterClient client.Client,
	insecureRegistry bool,
	config types.ImageSpec,
	filePath string,
) (any, error) {
	keyChain, err := configKeyChain(ctx, namespace, clusterClient, config)
	if err != nil {
		return nil, err
	}
	return descriptor.DecodeUncompressedLayer(config, insecureRegistry, keyChain, filePath)
}

func configKeyChain(ctx context.Context,
	namespace string,
	clusterClient client.Client,
	imageSpec types.ImageSpec,
) (authn.Keychain, error) {
	var keyChain authn.Keychain
	var err error
	if imageSpec.CredSecretSelector != nil {
		if keyChain, err = GetAuthnKeychain(ctx, imageSpec, clusterClient, namespace); err != nil {
			return nil, err
		}
	} else {
		keyChain = authn.DefaultKeychain
	}
	return keyChain, nil
}

func getDestinationConfigAndClient(ctx context.Context, defaultClusterInfo types.ClusterInfo,
	manifestObj *v1alpha1.Manifest, processorCache types.RendererCache, customCfgGetter internalTypes.RESTConfigGetter,
) (types.ClusterInfo, error) {
	// in single cluster mode return the default cluster info
	// since the resources need to be installed in the same cluster
	if !manifestObj.Spec.Remote {
		return defaultClusterInfo, nil
	}

	kymaOwnerLabel, err := util.GetResourceLabel(manifestObj, labels.CacheKey)
	if err != nil {
		return types.ClusterInfo{}, err
	}

	// cluster info record from cluster cache
	kymaNsName := client.ObjectKey{Name: kymaOwnerLabel, Namespace: manifestObj.Namespace}
	processor := processorCache.GetProcessor(kymaNsName)
	if processor != nil {
		return processor.GetClusterInfo()
	}

	// RESTConfig can either be retrieved by a secret with name contained in labels.ComponentOwner Manifest CR label,
	// or it can be retrieved as a function return value, passed during controller startup.
	var restConfigGetter internalTypes.RESTConfigGetter
	if customCfgGetter != nil {
		restConfigGetter = customCfgGetter
	} else {
		restConfigGetter = getDefaultRESTConfigGetter(ctx, kymaOwnerLabel, manifestObj.Namespace,
			defaultClusterInfo.Client)
	}
	restConfig, err := restConfigGetter()
	if err != nil {
		return types.ClusterInfo{}, err
	}

	return types.ClusterInfo{
		Config: restConfig,
		// client will be set during processing of manifest
	}, nil
}

func parseInstallations(ctx context.Context,
	manifestObj *v1alpha1.Manifest,
	codec *types.Codec,
	configs []interface{},
	baseDeployInfo *types.InstallInfo,
	insecureRegistry bool,
	clusterClient client.Client,
) ([]*types.InstallInfo, error) {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	deployInfos := make([]*types.InstallInfo, 0)

	for _, install := range manifestObj.Spec.Installs {
		deployInfo := baseDeployInfo

		// retrieve chart info
		chartInfo, err := getChartInfoForInstall(ctx, install, codec, manifestObj, insecureRegistry, clusterClient)
		if err != nil {
			return nil, err
		}

		// filter config for install
		chartConfig, chartValues, err := parseChartConfigAndValues(install, configs, namespacedName.String())
		if err != nil {
			return nil, err
		}

		// common deploy properties
		chartInfo.ReleaseName = install.Name
		chartInfo.Flags = types.ChartFlags{
			ConfigFlags: chartConfig,
			SetFlags:    chartValues,
		}

		deployInfo.ChartInfo = chartInfo
		deployInfos = append(deployInfos, deployInfo)
	}

	return deployInfos, nil
}

func parseCrds(ctx context.Context,
	manifestObj *v1alpha1.Manifest,
	insecureRegistry bool,
	clusterClient client.Client,
) ([]*v1.CustomResourceDefinition, error) {
	// if crds do not exist - do nothing
	if manifestObj.Spec.CRDs.Type.NotEmpty() {
		// extract helm chart from layer digest
		crdsPath, err := getChartPath(ctx, manifestObj.Spec.CRDs, manifestObj.Namespace, insecureRegistry, clusterClient)
		if err != nil {
			return nil, err
		}
		return resource.GetCRDsFromPath(ctx, crdsPath)
	}
	return nil, nil
}

func getChartPath(ctx context.Context,
	imageSpec types.ImageSpec,
	namespace string,
	insecureRegistry bool,
	clusterClient client.Client,
) (string, error) {
	keyChain, err := configKeyChain(ctx, namespace, clusterClient, imageSpec)
	if err != nil {
		return "", err
	}
	return descriptor.GetPathFromExtractedTarGz(imageSpec, insecureRegistry, keyChain)
}

func GetAuthnKeychain(ctx context.Context,
	imageSpec types.ImageSpec,
	clusterClient client.Client,
	namespace string,
) (authn.Keychain, error) {
	secretList, err := getCredSecrets(ctx, imageSpec.CredSecretSelector, clusterClient, namespace)
	if err != nil {
		return nil, err
	}
	return authnK8s.NewFromPullSecrets(ctx, secretList.Items)
}

func getCredSecrets(ctx context.Context,
	credSecretSelector *metav1.LabelSelector,
	clusterClient client.Client,
	namespace string,
) (corev1.SecretList, error) {
	secretList := corev1.SecretList{}
	selector, err := metav1.LabelSelectorAsSelector(credSecretSelector)
	if err != nil {
		return secretList, fmt.Errorf("error converting labelSelector: %w", err)
	}
	err = clusterClient.List(ctx, &secretList, &client.ListOptions{
		LabelSelector: selector,
		Namespace:     namespace,
	})
	if err != nil {
		return secretList, err
	}
	if len(secretList.Items) == 0 {
		return secretList, ErrNoAuthSecretFound
	}
	return secretList, nil
}

func getChartInfoForInstall(ctx context.Context,
	install v1alpha1.InstallInfo,
	codec *types.Codec,
	manifestObj *v1alpha1.Manifest,
	insecureRegistry bool,
	clusterClient client.Client,
) (*types.ChartInfo, error) {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	specType, err := types.GetSpecType(install.Source.Raw)
	if err != nil {
		return nil, err
	}

	switch specType {
	case types.HelmChartType:
		return createHelmChartInfo(codec, install, specType)
	case types.OciRefType:
		return createOciChartInfo(ctx, install, codec, specType, manifestObj, insecureRegistry, clusterClient)
	case types.KustomizeType:
		return createKustomizeChartInfo(codec, install, specType)
	case types.NilRefType:
		return nil, fmt.Errorf("empty image type for %s resource chart installation", namespacedName.String())
	}

	return nil, fmt.Errorf("unsupported type %s of install for Manifest %s", specType, namespacedName)
}

func createKustomizeChartInfo(codec *types.Codec,
	install v1alpha1.InstallInfo,
	specType types.RefTypeMetadata,
) (*types.ChartInfo, error) {
	var kustomizeSpec types.KustomizeSpec
	if err := codec.Decode(install.Source.Raw, &kustomizeSpec, specType); err != nil {
		return nil, err
	}

	return &types.ChartInfo{
		ChartName: install.Name,
		ChartPath: kustomizeSpec.Path,
		URL:       kustomizeSpec.URL,
	}, nil
}

func createOciChartInfo(ctx context.Context,
	install v1alpha1.InstallInfo,
	codec *types.Codec,
	specType types.RefTypeMetadata,
	manifestObj *v1alpha1.Manifest,
	insecureRegistry bool,
	clusterClient client.Client,
) (*types.ChartInfo, error) {
	var imageSpec types.ImageSpec
	if err := codec.Decode(install.Source.Raw, &imageSpec, specType); err != nil {
		return nil, err
	}

	// extract helm chart from layer digest
	chartPath, err := getChartPath(ctx, imageSpec, manifestObj.Namespace, insecureRegistry, clusterClient)
	if err != nil {
		return nil, err
	}

	return &types.ChartInfo{
		ChartName: install.Name,
		ChartPath: chartPath,
	}, nil
}

func createHelmChartInfo(codec *types.Codec,
	install v1alpha1.InstallInfo,
	specType types.RefTypeMetadata,
) (*types.ChartInfo, error) {
	var helmChartSpec types.HelmChartSpec
	if err := codec.Decode(install.Source.Raw, &helmChartSpec, specType); err != nil {
		return nil, err
	}

	return &types.ChartInfo{
		ChartName: fmt.Sprintf("%s/%s", install.Name, helmChartSpec.ChartName),
		RepoName:  install.Name,
		URL:       helmChartSpec.URL,
	}, nil
}

func getConfigAndValuesForInstall(installName string, configs []interface{}) (
	string, string, error,
) {
	var defaultOverrides string
	var clientConfig string

	for _, config := range configs {
		mappedConfig, configExists := config.(map[string]interface{})
		if !configExists {
			return "", "", fmt.Errorf(configReadError, "config object")
		}
		if mappedConfig["name"] == installName {
			defaultOverrides, configExists = mappedConfig["overrides"].(string)
			if !configExists {
				return "", "", fmt.Errorf(configReadError, "config object overrides")
			}
			clientConfig, configExists = mappedConfig["clientConfig"].(string)
			if !configExists {
				return "", "", fmt.Errorf(configReadError, "chart config")
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
	configString, valuesString, err := getConfigAndValuesForInstall(install.Name, configs)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest %s encountered an error while parsing chart config: %w", namespacedName, err)
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

// InsertWatcherLabels adds watcher labels to custom resource of the Manifest CR.
func InsertWatcherLabels(manifestObj *v1alpha1.Manifest) {
	// Make sure Manifest CR is enabled for remote and Spec.Resource is a valid resource
	if !manifestObj.Spec.Remote || manifestObj.Spec.Resource.GetKind() == "" {
		return
	}

	manifestLabels := manifestObj.Spec.Resource.GetLabels()

	ownedByValue := fmt.Sprintf(labels.OwnedByFormat, manifestObj.Namespace, manifestObj.Name)

	if manifestLabels == nil {
		manifestLabels = make(map[string]string)
	}

	manifestLabels[labels.OwnedByLabel] = ownedByValue
	manifestLabels[labels.WatchedByLabel] = labels.OperatorName

	manifestObj.Spec.Resource.SetLabels(manifestLabels)
}

func getDefaultRESTConfigGetter(ctx context.Context, secretName string, namespace string,
	client client.Client,
) internalTypes.RESTConfigGetter {
	return func() (*rest.Config, error) {
		// evaluate remote rest config from secret
		clusterClient := &custom.ClusterClient{DefaultClient: client}
		return clusterClient.GetRESTConfig(ctx, secretName, namespace)
	}
}
