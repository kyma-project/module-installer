package manifest

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/cli"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kyma-project/module-manager/operator/pkg/labels"
	"github.com/kyma-project/module-manager/operator/pkg/resource"
	manifestRest "github.com/kyma-project/module-manager/operator/pkg/rest"
	"github.com/kyma-project/module-manager/operator/pkg/types"
	"github.com/kyma-project/module-manager/operator/pkg/util"
)

// ResourceInfo represents additional resources.
type ResourceInfo struct {
	// BaseResource represents base custom resource that is being reconciled
	BaseResource *unstructured.Unstructured
	// CustomResources represents a set of additional custom resources to be installed
	CustomResources []*unstructured.Unstructured
	// Crds represents a set of additional custom resource definitions to be installed
	Crds []*apiextensions.CustomResourceDefinition
}

type operations struct {
	logger             *logr.Logger
	renderSrc          types.RenderSrc
	flags              types.ChartFlags
	resourceTransforms []types.ObjectTransform
}

// InstallChart installs the resources based on types.InstallInfo and an appropriate rendering mechanism.
func InstallChart(logger *logr.Logger, deployInfo types.InstallInfo, resourceTransforms []types.ObjectTransform,
	cache types.RendererCache,
) (bool, error) {
	ops, err := newOperations(logger, deployInfo, resourceTransforms, cache)
	if err != nil {
		return false, err
	}

	return ops.install(deployInfo)
}

// UninstallChart uninstalls the resources based on types.InstallInfo and an appropriate rendering mechanism.
func UninstallChart(logger *logr.Logger, deployInfo types.InstallInfo, resourceTransforms []types.ObjectTransform,
	cache types.RendererCache,
) (bool, error) {
	ops, err := newOperations(logger, deployInfo, resourceTransforms, cache)
	if err != nil {
		return false, err
	}

	return ops.uninstall(deployInfo)
}

// ConsistencyCheck verifies consistency of resources based on types.InstallInfo and an appropriate rendering mechanism.
func ConsistencyCheck(logger *logr.Logger, deployInfo types.InstallInfo, resourceTransforms []types.ObjectTransform,
	cache types.RendererCache,
) (bool, error) {
	ops, err := newOperations(logger, deployInfo, resourceTransforms, cache)
	if err != nil {
		return false, err
	}

	return ops.consistencyCheck(deployInfo)
}

func newOperations(logger *logr.Logger, deployInfo types.InstallInfo, resourceTransforms []types.ObjectTransform,
	cache types.RendererCache,
) (*operations, error) {
	cacheKey := discoverCacheKey(deployInfo.BaseResource, logger)

	var renderSrc types.RenderSrc
	if cache != nil && cacheKey.Name != "" {
		// read manifest renderer from cache
		renderSrc = cache.Get(cacheKey)
	}
	// cache entry not found
	if renderSrc == nil {
		memCacheClient, err := getMemCacheClient(deployInfo.Config)
		if err != nil {
			return nil, err
		}
		render := NewRendered(logger)
		txformer := NewTransformer()
		renderSrc, err = getManifestProcessor(deployInfo, memCacheClient, logger, render, txformer)
		if err != nil {
			return nil, fmt.Errorf("unable to create manifest processor: %w", err)
		}
		if cache != nil && cacheKey.Name != "" {
			// cache manifest renderer
			cache.Set(cacheKey, renderSrc)
		}
	}

	ops := &operations{
		logger:             logger,
		renderSrc:          renderSrc,
		flags:              deployInfo.Flags,
		resourceTransforms: resourceTransforms,
	}

	return ops, nil
}

// discoverCacheKey returns cache key for caching of manifest renderer,
// by label value operator.kyma-project.io/cache-key.
// If label not found on base resource an empty cache key is returned.
func discoverCacheKey(resource client.Object, logger *logr.Logger) client.ObjectKey {
	if resource != nil {
		label, err := util.GetResourceLabel(resource, labels.CacheKey)
		var labelErr *util.LabelNotFoundError
		if errors.As(err, &labelErr) {
			logger.V(util.DebugLogLevel).Info("cache-key label missing, resource will not be cached. Resulted in",
				"error", err.Error(),
				"resource", client.ObjectKeyFromObject(resource))
		}
		// do not handle any other error if reported
		return client.ObjectKey{Name: label, Namespace: resource.GetNamespace()}
	}
	return client.ObjectKey{}
}

// getManifestProcessor returns a new types.RenderSrc instance
// this render source will handle subsequent operations for manifest resources based on types.InstallInfo.
func getManifestProcessor(deployInfo types.InstallInfo, memCacheClient discovery.CachedDiscoveryInterface,
	logger *logr.Logger, render *rendered, txformer *transformer,
) (types.RenderSrc, error) {
	// use deferred discovery client here as GVs applicable to the client are inconsistent at this moment
	discoveryMapper := restmapper.NewDeferredDiscoveryRESTMapper(memCacheClient)

	chartKind, err := resource.GetChartKind(deployInfo)
	if err != nil {
		return nil, err
	}
	switch chartKind {
	case resource.HelmKind, resource.UnknownKind:
		// create RESTGetter with cached memcached client
		restGetter := manifestRest.NewRESTClientGetter(deployInfo.Config, memCacheClient)

		// create HelmClient instance
		return NewHelmProcessor(restGetter, discoveryMapper, deployInfo.Config, cli.New(), logger,
			render, txformer)
	case resource.KustomizeKind:
		// create dynamic client for rest config
		dynamicClient, err := dynamic.NewForConfig(deployInfo.Config)
		if err != nil {
			return nil, fmt.Errorf("error creating dynamic client: %w", err)
		}
		return NewKustomizeProcessor(dynamicClient, discoveryMapper, logger,
			render, txformer)
	}
	return nil, nil
}

// getMemCacheClient creates and returns a new instance of MemCacheClient.
func getMemCacheClient(config *rest.Config) (discovery.CachedDiscoveryInterface, error) {
	// The more groups you have, the more discovery requests you need to make.
	// given 25 groups (our groups + a few custom conf) with one-ish version each, discovery needs to make 50 requests
	// double it just so we don't end up here again for a while.  This config is only used for discovery.
	config.Burst = 100
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(discoveryClient), nil
}

func (o *operations) consistencyCheck(deployInfo types.InstallInfo) (bool, error) {
	// verify CRDs
	if err := resource.CheckCRDs(deployInfo.Ctx, deployInfo.Crds, deployInfo.ClusterInfo.Client,
		false); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// verify CR
	if err := resource.CheckCRs(deployInfo.Ctx, deployInfo.CustomResources, deployInfo.ClusterInfo.Client,
		false); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// process manifest
	manifest, err := o.getManifestForChartPath(deployInfo)
	if err != nil {
		return false, err
	}

	// consistency check
	consistent, err := o.renderSrc.IsConsistent(manifest, deployInfo, o.resourceTransforms)
	if err != nil || !consistent {
		return false, err
	}

	// custom states check
	if deployInfo.CheckFn != nil {
		return deployInfo.CheckFn(deployInfo.Ctx, deployInfo.BaseResource, o.logger, deployInfo.ClusterInfo)
	}
	return true, nil
}

func (o *operations) install(deployInfo types.InstallInfo) (bool, error) {
	// install crds first - if present do not update!
	if err := resource.CheckCRDs(deployInfo.Ctx, deployInfo.Crds, deployInfo.ClusterInfo.Client,
		true); err != nil {
		return false, err
	}

	// process manifest
	manifest, err := o.getManifestForChartPath(deployInfo)
	if err != nil {
		return false, err
	}

	// install resources
	consistent, err := o.renderSrc.Install(manifest, deployInfo, o.resourceTransforms)
	if err != nil || !consistent {
		return false, err
	}

	// install crs - if present do not update!
	if err := resource.CheckCRs(deployInfo.Ctx, deployInfo.CustomResources, deployInfo.Client,
		true); err != nil {
		return false, err
	}

	// custom states check
	if deployInfo.CheckFn != nil {
		return deployInfo.CheckFn(deployInfo.Ctx, deployInfo.BaseResource, o.logger, deployInfo.ClusterInfo)
	}
	return true, nil
}

func (o *operations) uninstall(deployInfo types.InstallInfo) (bool, error) {
	// delete crs first - proceed only if not found
	// proceed if CR type doesn't exist anymore - since associated CRDs might be deleted from resource uninstallation
	// since there might be a deletion process to be completed by other manifest resources
	deleted, err := resource.RemoveCRs(deployInfo.Ctx, deployInfo.CustomResources,
		deployInfo.ClusterInfo.Client)
	if !meta.IsNoMatchError(err) && (err != nil || !deleted) {
		return false, err
	}

	// process manifest
	manifest, err := o.getManifestForChartPath(deployInfo)
	if err != nil {
		return false, err
	}

	// uninstall resources
	consistent, err := o.renderSrc.Install(manifest, deployInfo, o.resourceTransforms)
	if err != nil || !consistent {
		return false, err
	}

	// delete crds first - if not present ignore!
	if err := resource.RemoveCRDs(deployInfo.Ctx, deployInfo.Crds, deployInfo.ClusterInfo.Client); err != nil {
		return false, err
	}

	// custom states check
	if deployInfo.CheckFn != nil {
		return deployInfo.CheckFn(deployInfo.Ctx, deployInfo.BaseResource, o.logger, deployInfo.ClusterInfo)
	}
	return true, err
}

func (o *operations) getManifestForChartPath(deployInfo types.InstallInfo) (string, error) {
	// 1. check provided manifest
	renderedManifest, err := o.renderSrc.GetManifestResources(deployInfo.ChartName, deployInfo.ChartPath)
	if err != nil {
		return "", err
	} else if renderedManifest != "" {
		return renderedManifest, nil
	}

	// 2. check cached manifest
	renderedManifest, err = o.renderSrc.GetCachedResources(deployInfo.ChartName, deployInfo.ChartPath)
	if err != nil {
		return "", err
	} else if renderedManifest != "" {
		return renderedManifest, nil
	}

	// 3. render manifests
	renderedManifest, err = o.renderSrc.GetRawManifest(deployInfo)
	if err != nil {
		return "", err
	}

	// 4. persist only if static chart path was passed
	if deployInfo.ChartPath != "" {
		err = util.WriteToFile(util.GetFsManifestChartPath(deployInfo.ChartPath), []byte(renderedManifest))
		if err != nil {
			if !os.IsPermission(err) {
				return renderedManifest, err
			}
			o.logger.Info("manifest for chart path could not be cached, this will affect performance",
				"resource", client.ObjectKeyFromObject(deployInfo.BaseResource).String())
		}
	}

	// optional: Uncomment below to print manifest
	// fmt.Println(release.Manifest)

	return renderedManifest, nil
}
