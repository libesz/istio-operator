package controlplane

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/util/sets"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/maistra/istio-operator/pkg/apis/maistra/status"
	v2 "github.com/maistra/istio-operator/pkg/apis/maistra/v2"
	"github.com/maistra/istio-operator/pkg/controller/common"
	"github.com/maistra/istio-operator/pkg/controller/common/cni"
)

const (
	controllerName = "servicemeshcontrolplane-controller"
)

// Add creates a new ControlPlane Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	operatorNamespace := common.GetOperatorNamespace()
	cniConfig, err := cni.InitConfig(mgr)
	if err != nil {
		return err
	}

	// TODO: Change kubeconfigPath from fixed value to configurable solution
	secondaryKubeConfig, err := clientcmd.BuildConfigFromFlags("", "/etc/kubeconfigs/secondary.yaml")
	if err != nil {
		return err
	}
	secondaryKubeConfig.Burst = common.Config.Controller.APIBurst * 2
	secondaryKubeConfig.QPS = common.Config.Controller.APIQPS * 2

	secondaryKubeClient, err := client.New(secondaryKubeConfig, client.Options{})
	if err != nil {
		return err
	}

	reconciler := newReconciler(mgr.GetClient(), mgr.GetScheme(), mgr.GetEventRecorderFor(controllerName), operatorNamespace, cniConfig, secondaryKubeConfig, secondaryKubeClient)
	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(client client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder, operatorNamespace string, cniConfig cni.Config, secondaryKubeConfig *rest.Config, secondaryClient client.Client) *ControlPlaneReconciler {
	reconciler := &ControlPlaneReconciler{
		ControllerResources: common.ControllerResources{
			Client:            client,
			Scheme:            scheme,
			EventRecorder:     eventRecorder,
			OperatorNamespace: operatorNamespace,
		},
		cniConfig:   cniConfig,
		secondaryKubeConfig: secondaryKubeConfig,
		secondaryKubeClient: secondaryClient,
		reconcilers: map[types.NamespacedName]ControlPlaneInstanceReconciler{},
	}
	reconciler.instanceReconcilerFactory = NewControlPlaneInstanceReconciler
	return reconciler
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ControlPlaneReconciler) error {
	log := createLogger()
	ctx := common.NewContextWithLog(common.NewContext(), log)

	wrappedReconciler := common.NewConflictHandlingReconciler(r)
	// Create a new controller
	var c controller.Controller
	var err error
	if c, err = controller.New(controllerName, mgr, controller.Options{MaxConcurrentReconciles: common.Config.Controller.ControlPlaneReconcilers, Reconciler: wrappedReconciler}); err != nil {
		return err
	}

	// Watch for changes to primary resource ServiceMeshControlPlane
	if err = c.Watch(&source.Kind{Type: &v2.ServiceMeshControlPlane{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}

	// watch created resources for use in synchronizing ready status
	if err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &v2.ServiceMeshControlPlane{},
		},
		ownedResourcePredicates); err != nil {
		return err
	}

	if err = c.Watch(&source.Kind{Type: &appsv1.StatefulSet{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &v2.ServiceMeshControlPlane{},
		},
		ownedResourcePredicates); err != nil {
		return err
	}

	if err = c.Watch(&source.Kind{Type: &appsv1.DaemonSet{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &v2.ServiceMeshControlPlane{},
		},
		ownedResourcePredicates); err != nil {
		return err
	}

	watchExternalObjects(ctx, r)

	// add watch for cni daemon set
	operatorNamespace := common.GetOperatorNamespace()
	if err = c.Watch(&source.Kind{Type: &appsv1.DaemonSet{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(func(obj handler.MapObject) []reconcile.Request {
				if obj.Meta.GetNamespace() != operatorNamespace {
					return nil
				}
				smcpList := &v2.ServiceMeshControlPlaneList{}
				if err := mgr.GetClient().List(ctx, smcpList); err != nil {
					log.Error(err, "error listing ServiceMeshControlPlane objects in CNI DaemonSet watcher")
					return nil
				}
				requests := make([]reconcile.Request, 0, len(smcpList.Items))
				for _, smcp := range smcpList.Items {
					requests = append(requests, reconcile.Request{
						NamespacedName: common.ToNamespacedName(&smcp),
					})
				}
				return requests
			}),
		},
		ownedResourcePredicates); err != nil {
		return err
	}

	return nil
}

// TODO: This is a PoC code with fixed values
func watchExternalObjects(ctx context.Context, reconciler *ControlPlaneReconciler) {
	log := common.LogFromContext(ctx)

	clientset, err := kubernetes.NewForConfig(reconciler.secondaryKubeConfig)
	if err != nil {
		log.Info(fmt.Sprintf("Error while creating external watch clientset: %s", err))
	}

	optionsModifer := func(options *metav1.ListOptions) {
		options.LabelSelector = "maistra.io/owner=external-istiod"
	}

	watchlist := cache.NewFilteredListWatchFromClient(clientset.AppsV1().RESTClient(), "deployments", v1.NamespaceAll, optionsModifer)
	_, controller := cache.NewInformer(
		watchlist,
		&appsv1.Deployment{},
		time.Second * 0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Info(fmt.Sprintf("XXX EXTERNAL deployment added: %s \n", obj))
			},
			DeleteFunc: func(obj interface{}) {
				log.Info(fmt.Sprintf("XXX EXTERNAL deployment deleted: %s \n", obj))
			},
			UpdateFunc:func(oldObj, newObj interface{}) {
				log.Info(fmt.Sprintf("XXX EXTERNAL deployment changed \n"))
				for _ , rec := range reconciler.reconcilers {
					_, err = rec.Reconcile(ctx)
					if err != nil {
						return
					}
				}
			},
		},
	)

	stop := make(chan struct{})
	go controller.Run(stop)
}

var ownedResourcePredicates = predicate.Funcs{
	CreateFunc: func(_ event.CreateEvent) bool {
		// we don't need to update status on create events
		return false
	},
	GenericFunc: func(_ event.GenericEvent) bool {
		// we don't need to update status on generic events
		return false
	},
}

var _ reconcile.Reconciler = &ControlPlaneReconciler{}

// ControlPlaneReconciler handles reconciliation of ServiceMeshControlPlane
// objects. It creates a ControlPlaneInstanceReconciler for each instance.
type ControlPlaneReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	common.ControllerResources
	cniConfig cni.Config
	secondaryKubeConfig *rest.Config
	secondaryKubeClient client.Client

	reconcilers map[types.NamespacedName]ControlPlaneInstanceReconciler
	mu          sync.Mutex

	instanceReconcilerFactory       func(common.ControllerResources, client.Client, *v2.ServiceMeshControlPlane, cni.Config) ControlPlaneInstanceReconciler
}

// ControlPlaneInstanceReconciler reconciles a specific instance of a ServiceMeshControlPlane
type ControlPlaneInstanceReconciler interface {
	Reconcile(ctx context.Context) (reconcile.Result, error)
	UpdateReadiness(ctx context.Context) error
	PatchAddons(ctx context.Context) error
	Delete(ctx context.Context) error
	SetInstance(instance *v2.ServiceMeshControlPlane)
	IsFinished() bool
}

// Reconcile reads that state of the cluster for a ServiceMeshControlPlane object and makes changes based on the state read
// and what is in the ServiceMeshControlPlane.Spec
func (r *ControlPlaneReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log := createLogger().WithValues("ServiceMeshControlPlane", request)
	ctx := common.NewReconcileContext(log)

	log.Info("Processing ServiceMeshControlPlane")
	defer func() {
		log.Info("Completed ServiceMeshControlPlane processing")
	}()

	// Fetch the ServiceMeshControlPlane instance
	instance := &v2.ServiceMeshControlPlane{}
	err := r.Client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) || errors.IsGone(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("ServiceMeshControlPlane deleted")
			return reconcile.Result{}, nil
		}
		// Error reading the object
		return reconcile.Result{}, err
	}

	key, reconciler := r.getOrCreateReconciler(instance, r.secondaryKubeClient)
	defer r.deleteReconcilerIfFinished(key, reconciler)

	deleted := instance.GetDeletionTimestamp() != nil
	finalizers := sets.NewString(instance.Finalizers...)

	if deleted {
		if !finalizers.Has(common.FinalizerName) {
			log.Info("Deletion of ServiceMeshControlPlane complete")
			return reconcile.Result{}, nil
		}
		err := reconciler.Delete(ctx)
		return reconcile.Result{}, err
	} else if !finalizers.Has(common.FinalizerName) {
		log.V(1).Info("Adding finalizer", "finalizer", common.FinalizerName)
		finalizers.Insert(common.FinalizerName)
		instance.SetFinalizers(finalizers.List())
		err = r.Client.Update(ctx, instance)
		return reconcile.Result{}, err
	}

	if isFullyReconciled(instance) {
		err := reconciler.UpdateReadiness(ctx)
		if err == nil {
			err = reconciler.PatchAddons(ctx)
		}
		return reconcile.Result{}, err
	}

	return reconciler.Reconcile(ctx)
}

func isFullyReconciled(instance *v2.ServiceMeshControlPlane) bool {
	return status.CurrentReconciledVersion(instance.GetGeneration()) == instance.Status.GetReconciledVersion() &&
		instance.Status.GetCondition(status.ConditionTypeReconciled).Status == status.ConditionStatusTrue
}

func (r *ControlPlaneReconciler) getOrCreateReconciler(newInstance *v2.ServiceMeshControlPlane, cpCl client.Client) (types.NamespacedName, ControlPlaneInstanceReconciler) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := common.ToNamespacedName(newInstance)
	if reconciler, ok := r.reconcilers[key]; ok {
		reconciler.SetInstance(newInstance)
		return key, reconciler
	}

	newReconciler := r.instanceReconcilerFactory(r.ControllerResources, cpCl, newInstance, r.cniConfig)
	r.reconcilers[key] = newReconciler
	return key, newReconciler
}

func (r *ControlPlaneReconciler) deleteReconcilerIfFinished(key types.NamespacedName, reconciler ControlPlaneInstanceReconciler) {
	if reconciler == nil {
		return
	}
	if reconciler.IsFinished() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.reconcilers, key)
	}
}

// Don't use this function to obtain a logger. Get it by invoking
// common.LogFromContext(ctx) to ensure that the logger has the
// correct context info and logs it.
func createLogger() logr.Logger {
	return logf.Log.WithName(controllerName)
}
