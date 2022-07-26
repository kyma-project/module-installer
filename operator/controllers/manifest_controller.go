/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kyma-project/kyma-watcher/kcp/pkg/listener"
	"github.com/kyma-project/manifest-operator/operator/internal/pkg/prepare"
	"github.com/kyma-project/manifest-operator/operator/internal/pkg/util"
	"github.com/kyma-project/manifest-operator/operator/pkg/ratelimit"
	"github.com/kyma-project/manifest-operator/operator/pkg/types"

	"github.com/go-logr/logr"
	"github.com/kyma-project/manifest-operator/operator/api/v1alpha1"
	"github.com/kyma-project/manifest-operator/operator/pkg/labels"
	"github.com/kyma-project/manifest-operator/operator/pkg/manifest"
	"golang.org/x/time/rate"
	"helm.sh/helm/v3/pkg/cli"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const componentName = "manifest"

type RequeueIntervals struct {
	Success time.Duration
	Failure time.Duration
	Waiting time.Duration
}

type ManifestDeploy struct {
	Info         manifest.DeployInfo
	Mode         manifest.Mode
	ResponseChan manifest.ResponseChan
}

// ManifestReconciler reconciles a Manifest object.
type ManifestReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	RestConfig              *rest.Config
	RestMapper              *restmapper.DeferredDiscoveryRESTMapper
	DeployChan              chan ManifestDeploy
	Workers                 *ManifestWorkerPool
	RequeueIntervals        RequeueIntervals
	MaxConcurrentReconciles int
	VerifyInstallation      bool
	CustomStateCheck        bool
	Codec                   *types.Codec
}

//+kubebuilder:rbac:groups=component.kyma-project.io,resources=manifests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=component.kyma-project.io,resources=manifests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=component.kyma-project.io,resources=manifests/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch;get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ManifestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.NamespacedName.String())
	logger.Info("Reconciliation loop starting for", "resource", req.NamespacedName.String())

	// get manifest object
	manifestObj := v1alpha1.Manifest{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name, Namespace: req.Namespace}, &manifestObj); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		logger.Info(fmt.Sprintf("%s got deleted", req.NamespacedName.String()))
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	manifestObj = *manifestObj.DeepCopy()

	randomizeDuration := func(input time.Duration) time.Duration {
		millis := int(input / time.Millisecond)
		res := ratelimit.RandomizeByTenPercent(millis)
		return time.Duration(res) * time.Millisecond
	}

	// check if deletionTimestamp is set, retry until it gets fully deleted
	if !manifestObj.DeletionTimestamp.IsZero() && manifestObj.Status.State != v1alpha1.ManifestStateDeleting {
		// if the status is not yet set to deleting, also update the status
		return ctrl.Result{}, r.updateManifestStatus(ctx, &manifestObj, v1alpha1.ManifestStateDeleting,
			"deletion timestamp set")
	}

	// check finalizer on native object
	if !controllerutil.ContainsFinalizer(&manifestObj, labels.ManifestFinalizer) {
		controllerutil.AddFinalizer(&manifestObj, labels.ManifestFinalizer)
		return ctrl.Result{}, r.updateManifest(ctx, &manifestObj)
	}

	// state handling
	switch manifestObj.Status.State {
	case "":
		return ctrl.Result{}, r.HandleInitialState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateProcessing:
		return ctrl.Result{RequeueAfter: randomizeDuration(r.RequeueIntervals.Failure)}, r.HandleProcessingState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateDeleting:
		return ctrl.Result{}, r.HandleDeletingState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateError:
		return ctrl.Result{RequeueAfter: randomizeDuration(r.RequeueIntervals.Failure)},
			r.HandleErrorState(ctx, &manifestObj)
	case v1alpha1.ManifestStateReady:
		return ctrl.Result{RequeueAfter: randomizeDuration(r.RequeueIntervals.Success)},
			r.HandleReadyState(ctx, &logger, &manifestObj)
	}

	// should not be reconciled again
	return ctrl.Result{}, nil
}

func (r *ManifestReconciler) HandleInitialState(ctx context.Context, _ *logr.Logger, manifestObj *v1alpha1.Manifest,
) error {
	return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing, "initial state")
}

func (r *ManifestReconciler) HandleProcessingState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest,
) error {
	return r.jobAllocator(ctx, logger, manifestObj, manifest.CreateMode)
}

func (r *ManifestReconciler) HandleDeletingState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest,
) error {
	return r.jobAllocator(ctx, logger, manifestObj, manifest.DeletionMode)
}

func (r *ManifestReconciler) jobAllocator(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest,
	mode manifest.Mode) error {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	responseChan := make(manifest.ResponseChan)

	chartCount := len(manifestObj.Spec.Installs)

	// response handler in a separate go-routine
	go r.ResponseHandlerFunc(ctx, logger, chartCount, responseChan, namespacedName)

	// send deploy requests
	deployInfos, err := prepare.GetDeployInfos(ctx, manifestObj, r.Client, r.VerifyInstallation,
		r.CustomStateCheck, r.Codec, r.RestConfig)
	if err != nil {
		return err
	}

	for _, deployInfo := range deployInfos {
		r.DeployChan <- ManifestDeploy{
			Info:         deployInfo,
			Mode:         mode,
			ResponseChan: responseChan,
		}
	}
	return nil
}

func (r *ManifestReconciler) HandleErrorState(ctx context.Context, manifestObj *v1alpha1.Manifest) error {
	return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing,
		"observed generation change")
}

func (r *ManifestReconciler) HandleReadyState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest,
) error {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	if manifestObj.Generation != manifestObj.Status.ObservedGeneration {
		logger.Info("observed generation change for " + namespacedName.String())
		return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing,
			"observed generation change")
	}

	logger.Info("checking consistent state for " + namespacedName.String())

	// send deploy requests
	deployInfos, err := prepare.GetDeployInfos(ctx, manifestObj, r.Client, r.VerifyInstallation, r.CustomStateCheck,
		r.Codec, r.RestConfig)
	if err != nil {
		return err
	}

	for _, deployInfo := range deployInfos {
		args := prepareArgs(&deployInfo)
		manifestOperations, err := manifest.NewOperations(logger, deployInfo.RestConfig, deployInfo.ReleaseName,
			cli.New(), args)
		if err != nil {
			logger.Error(err, fmt.Sprintf("error while creating library operations for manifest %s", namespacedName))
			return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateError, err.Error())
		}

		if ready, err := manifestOperations.VerifyResources(deployInfo); !ready {
			return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing,
				"resources not ready")
		} else if err != nil {
			logger.Error(err, fmt.Sprintf("error while performing consistency check on manifest %s", namespacedName))
			return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateError, err.Error())
		}
	}
	return nil
}

func (r *ManifestReconciler) updateManifest(ctx context.Context, manifestObj *v1alpha1.Manifest) error {
	return r.Update(ctx, manifestObj)
}

func (r *ManifestReconciler) updateManifestStatus(ctx context.Context, manifestObj *v1alpha1.Manifest, state v1alpha1.ManifestState, message string,
) error {
	manifestObj.Status.State = state
	switch state {
	case v1alpha1.ManifestStateReady:
		util.AddReadyConditionForObjects(manifestObj, []v1alpha1.InstallItem{{ChartName: v1alpha1.ManifestKind}},
			v1alpha1.ConditionStatusTrue, message)
	case "":
		util.AddReadyConditionForObjects(manifestObj, []v1alpha1.InstallItem{{ChartName: v1alpha1.ManifestKind}},
			v1alpha1.ConditionStatusUnknown, message)
	default:
		util.AddReadyConditionForObjects(manifestObj, []v1alpha1.InstallItem{{ChartName: v1alpha1.ManifestKind}},
			v1alpha1.ConditionStatusFalse, message)
	}
	return r.Status().Update(ctx, manifestObj.SetObservedGeneration())
}

func (r *ManifestReconciler) HandleCharts(deployInfo manifest.DeployInfo, mode manifest.Mode, logger *logr.Logger,
) *manifest.ChartResponse {
	args := prepareArgs(&deployInfo)

	// evaluate create or delete chart
	create := mode == manifest.CreateMode

	var ready bool
	// TODO: implement better settings handling
	manifestOperations, err := manifest.NewOperations(logger, deployInfo.RestConfig, deployInfo.ReleaseName,
		cli.New(), args)

	if err == nil {
		if create {
			ready, err = manifestOperations.Install(deployInfo)
		} else {
			ready, err = manifestOperations.Uninstall(deployInfo)
		}
	}

	return &manifest.ChartResponse{
		Ready:             ready,
		ResNamespacedName: deployInfo.ObjectKey,
		Err:               err,
		ChartName:         deployInfo.ChartName,
		ClientConfig:      deployInfo.ClientConfig,
		Overrides:         deployInfo.Overrides,
	}
}

func (r *ManifestReconciler) ResponseHandlerFunc(ctx context.Context, logger *logr.Logger, chartCount int,
	responseChan manifest.ResponseChan, namespacedName client.ObjectKey,
) {
	// errorState takes precedence over processing
	errorState := false
	processing := false
	responses := make([]*manifest.ChartResponse, 0)

	for a := 1; a <= chartCount; a++ {
		select {
		case <-ctx.Done():
			logger.Error(ctx.Err(), fmt.Sprintf("context closed, error occurred while handling response for %s",
				namespacedName.String()))
			return
		case response := <-responseChan:
			responses = append(responses, response)
			if response.Err != nil {
				logger.Error(fmt.Errorf("chart installation failure for %s!!! : %w",
					response.ResNamespacedName.String(), response.Err), "")
				errorState = true
			} else if !response.Ready {
				logger.Info(fmt.Sprintf("chart checks still processing %s!!!",
					response.ResNamespacedName.String()))
				processing = true
			}
		}
	}

	latestManifestObj := &v1alpha1.Manifest{}
	if err := r.Get(ctx, namespacedName, latestManifestObj); err != nil {
		logger.Error(err, "error while locating", "resource", namespacedName)
		return
	}

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

		util.AddReadyConditionForObjects(latestManifestObj, []v1alpha1.InstallItem{{
			ClientConfig: string(configBytes),
			Overrides:    string(overrideBytes),
			ChartName:    response.ChartName,
		}}, status, message)
	}

	// handle deletion if no previous error occurred
	if !errorState && !latestManifestObj.DeletionTimestamp.IsZero() && !processing {
		if !errorState {
			// remove finalizer
			controllerutil.RemoveFinalizer(latestManifestObj, labels.ManifestFinalizer)
			if err := r.updateManifest(ctx, latestManifestObj); err != nil {
				// finalizer removal failure
				logger.Error(err, "unexpected error while removing finalizer from",
					"resource", namespacedName)
				errorState = true
			} else {
				// finalizer successfully removed
				return
			}
		}
	}

	endState := v1alpha1.ManifestStateDeleting
	if errorState {
		endState = v1alpha1.ManifestStateError
	} else if latestManifestObj.DeletionTimestamp.IsZero() {
		// only update to processing, ready if deletion has not been triggered
		if processing {
			endState = v1alpha1.ManifestStateProcessing
		} else {
			endState = v1alpha1.ManifestStateReady
		}
	}

	// update status for non-deletion scenarios
	if err := r.updateManifestStatus(ctx, latestManifestObj, endState,
		fmt.Sprintf("%s in %s state", v1alpha1.ManifestKind, endState)); err != nil {
		logger.Error(err, "error updating status", "resource", namespacedName)
	}
	return
}

func ManifestRateLimiter() ratelimiter.RateLimiter {
	return workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(1*time.Second, 1000*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(30), 200)})
}

func prepareArgs(deployInfo *manifest.DeployInfo) map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		// check --set flags parameter from manifest
		"set": deployInfo.Overrides,
		// comma separated values of manifest command line flags
		"flags": deployInfo.ClientConfig,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ManifestReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, listenerAddr string) error {
	r.DeployChan = make(chan ManifestDeploy)
	r.Workers.StartWorkers(ctx, r.DeployChan, r.HandleCharts)

	// default config from kubebuilder
	r.RestConfig = mgr.GetConfig()

	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Manifest{}).
		Watches(&source.Kind{Type: &v1.Secret{}}, handler.Funcs{}).
		WithOptions(controller.Options{
			RateLimiter:             ManifestRateLimiter(),
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
		})

	// register listener component
	runnableListener, eventChannel := listener.RegisterListenerComponent(listenerAddr, componentName)
	// watch event channel
	controllerBuilder.Watches(eventChannel, handler.EnqueueRequestsFromMapFunc(r.skrEventsMapFunc(ctx)))
	// start listener as a manager runnable
	if err := mgr.Add(runnableListener); err != nil {
		return err
	}

	return controllerBuilder.Complete(r)
}

func (r *ManifestReconciler) skrEventsMapFunc(ctx context.Context) handler.MapFunc {

	logger := log.FromContext(ctx).WithName("skr-watcher-events-processing")
	return func(o client.Object) []reconcile.Request {

		requests := make([]reconcile.Request, 0, 1)
		namespacedName := client.ObjectKeyFromObject(o)
		kymaName := o.GetLabels()[labels.ComponentOwner]
		manifestCRsForKymaName := &v1alpha1.ManifestList{}
		err := r.List(ctx, manifestCRsForKymaName, client.MatchingLabels{
			labels.ComponentOwner: kymaName,
		})
		if err != nil {
			logger.WithValues(
				"component", namespacedName.Name,
				"namespace", namespacedName.Namespace,
			).Error(err, "failed to list manifest CRs")
			return nil
		}

		manifestCRForComponent, err := findManifestForComponent(namespacedName, manifestCRsForKymaName.Items)
		if err != nil {
			logger.WithValues(
				"component", namespacedName.Name,
				"namespace", namespacedName.Namespace,
			).Error(err, "failed to find manifest CR")
			return nil
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(manifestCRForComponent),
		})
		return requests
	}
}

func findManifestForComponent(namespacedName client.ObjectKey, manifests []v1alpha1.Manifest) (*v1alpha1.Manifest, error) {
	for _, manifestCR := range manifests {
		equalObjectKeyPredicate := manifestCR.Spec.Resource.GetName() == namespacedName.Name &&
			manifestCR.Spec.Resource.GetNamespace() == namespacedName.Namespace
		if equalObjectKeyPredicate {
			return &manifestCR, nil
		}
	}
	return nil, fmt.Errorf("corresponding manifest for requested component not found")
}
