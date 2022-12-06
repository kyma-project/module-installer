package v2

import (
	"context"
	"fmt"
	"time"

	"github.com/kyma-project/module-manager/pkg/types"
	"github.com/kyma-project/module-manager/pkg/util"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/cli-runtime/pkg/resource"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SSA interface {
	Run(context.Context, []*resource.Info) error
}

type concurrentDefaultSSA struct {
	clnt      client.Client
	owner     client.FieldOwner
	versioner runtime.GroupVersioner
	converter runtime.ObjectConvertor
}

func ConcurrentSSA(clnt client.Client, owner client.FieldOwner) SSA {
	return &concurrentDefaultSSA{
		clnt: clnt, owner: owner,
		versioner: runtime.GroupVersioner(schema.GroupVersions(clnt.Scheme().PrioritizedVersionsAllGroups())),
		converter: clnt.Scheme(),
	}
}

func (c *concurrentDefaultSSA) Run(ctx context.Context, resources []*resource.Info) error {
	ssaStart := time.Now()
	logger := log.FromContext(ctx, "owner", c.owner)
	logger.V(util.TraceLogLevel).Info("ServerSideApply", "resources", len(resources))

	// Runtime Complexity of this Branch is N as only ServerSideApplier Patch is required
	results := make(chan error, len(resources))
	for i := range resources {
		i := i
		go c.serverSideApply(ctx, resources[i], results)
	}

	var errs []error
	for i := 0; i < len(resources); i++ {
		if err := <-results; err != nil {
			errs = append(errs, err)
		}
	}

	ssaFinish := time.Since(ssaStart)

	if errs != nil {
		return fmt.Errorf("ServerSideApply failed (after %s): %w", ssaFinish, types.NewMultiError(errs))
	}
	logger.V(util.DebugLogLevel).Info("ServerSideApply finished", "time", ssaFinish)
	return nil
}

func (c *concurrentDefaultSSA) serverSideApply(
	ctx context.Context,
	resource *resource.Info,
	results chan error,
) {
	start := time.Now()
	logger := log.FromContext(ctx, "owner", c.owner)

	// this converts unstructured to typed objects if possible, leveraging native APIs
	resource.Object = c.convertUnstructuredToTyped(resource.Object, resource.Mapping)

	logger.V(util.TraceLogLevel).Info(
		fmt.Sprintf("apply %s (%s)", resource.ObjectName(), resource.Mapping.GroupVersionKind))

	results <- c.serverSideApplyResourceInfo(ctx, resource)

	logger.V(util.TraceLogLevel).Info(
		fmt.Sprintf("apply %s (%s) finished", resource.ObjectName(), resource.Mapping.GroupVersionKind),
		"time", time.Since(start))
}

func (c *concurrentDefaultSSA) serverSideApplyResourceInfo(
	ctx context.Context,
	info *resource.Info,
) error {
	obj, isTyped := info.Object.(client.Object)
	if !isTyped {
		return fmt.Errorf("client object conversion for %s failed,"+
			"object is not a valid client-go object", info.ObjectName())
	}

	err := c.clnt.Patch(ctx, obj, client.Apply, client.ForceOwnership, c.owner)
	if err != nil {
		return fmt.Errorf("patch for %s (%s) failed: %w", info.ObjectName(),
			info.Mapping.GroupVersionKind, err)
	}

	return nil
}

// convertWithMapper converts the given object with the optional provided
// RESTMapping. If no mapping is provided, the default schema versioner is used.
func (c *concurrentDefaultSSA) convertUnstructuredToTyped(
	obj runtime.Object, mapping *meta.RESTMapping,
) runtime.Object {
	gv := c.versioner
	if mapping != nil {
		gv = mapping.GroupVersionKind.GroupVersion()
	}
	if obj, err := c.converter.ConvertToVersion(obj, gv); err == nil {
		return obj
	}
	return obj
}
