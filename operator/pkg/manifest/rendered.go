package manifest

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"

	"github.com/kyma-project/module-manager/operator/pkg/resource"
	"github.com/kyma-project/module-manager/operator/pkg/types"
	"github.com/kyma-project/module-manager/operator/pkg/util"
)

type rendered struct {
	logger *logr.Logger
}

// NewRendered returns a new instance on rendered.
// Using rendered instance, pre-rendered and cached manifest can be identified and retrieved.
//
//nolint:revive
func NewRendered(logger *logr.Logger) *rendered {
	return &rendered{
		logger: logger,
	}
}

// GetCachedResources returns a resource manifest which was already cached during previous operations
// by the module-manager library.
func (r *rendered) GetCachedResources(chartName, chartPath string) *types.ParsedFile {
	if emptyPath(chartPath) {
		return &types.ParsedFile{}
	}

	// verify chart path exists
	if _, err := os.Stat(chartPath); err != nil {
		return types.NewParsedFile("", err)
	}
	r.logger.Info(fmt.Sprintf("chart dir %s found at path %s", chartName, chartPath))

	// check if pre-rendered manifest already exists
	return types.NewParsedFile(util.GetStringifiedYamlFromFilePath(util.GetFsManifestChartPath(chartPath)))
}

// GetManifestResources returns a pre-rendered resource manifest located at the passed chartPath.
func (r *rendered) GetManifestResources(chartName, chartPath string) *types.ParsedFile {
	if emptyPath(chartPath) {
		return &types.ParsedFile{}
	}
	// return already rendered manifest here
	return types.NewParsedFile(resource.GetStringifiedYamlFromDirPath(chartPath, r.logger))
}

func emptyPath(dirPath string) bool {
	return dirPath == ""
}
