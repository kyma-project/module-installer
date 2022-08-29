package labels

const (
	OperatorPrefix    = "operator.kyma-project.io"
	Separator         = "/"
	ComponentOwner    = OperatorPrefix + Separator + "kyma-name"
	ManagedBy         = OperatorPrefix + Separator + "managed-by"
	LifecycleManager  = "lifecycle-manager"
	ManifestFinalizer = "operator.kyma-project.io/manifest"
)
