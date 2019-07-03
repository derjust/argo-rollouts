package controller

import (
	"fmt"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/argoproj/argo-rollouts/controller/metrics"
	clientset "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned"
	rolloutscheme "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned/scheme"
	informers "github.com/argoproj/argo-rollouts/pkg/client/informers/externalversions/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/rollout"
	"github.com/argoproj/argo-rollouts/service"
)

const controllerAgentName = "rollouts-controller"

const (
	// DefaultRolloutResyncPeriod Default time in seconds for rollout resync period
	DefaultRolloutResyncPeriod = 15 * 60

	// DefaultMetricsPort Default port to expose the metrics endpoint
	DefaultMetricsPort = 8090
)

// Manager is the controller implementation for Rollout resources
type Manager struct {
	metricsServer     *metrics.MetricsServer
	rolloutController *rollout.RolloutController
	serviceController *service.ServiceController

	rolloutSynced     cache.InformerSynced
	serviceSynced     cache.InformerSynced
	replicasSetSynced cache.InformerSynced

	rolloutWorkqueue workqueue.RateLimitingInterface
	serviceWorkqueue workqueue.RateLimitingInterface
}

// NewControllerManager returns a new rollout controller
func NewControllerManager(
	kubeclientset kubernetes.Interface,
	rolloutsclientset clientset.Interface,
	replicaSetInformer appsinformers.ReplicaSetInformer,
	servicesInformer coreinformers.ServiceInformer,
	rolloutsInformer informers.RolloutInformer,
	resyncPeriod time.Duration,
	metricsPort int) *Manager {

	utilruntime.Must(rolloutscheme.AddToScheme(scheme.Scheme))
	log.Info("Creating event broadcaster")

	// Create event broadcaster
	// Add rollouts-controller types to the default Kubernetes Scheme so Events can be
	// logged for argo-rollouts types.
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	metricsAddr := fmt.Sprintf("0.0.0.0:%d", metricsPort)

	rolloutWorkqueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Rollouts")
	serviceWorkqueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Services")

	metricsServer := metrics.NewMetricsServer(metricsAddr, rolloutsInformer.Lister())

	rolloutController := rollout.NewRolloutController(kubeclientset,
		rolloutsclientset,
		replicaSetInformer,
		servicesInformer,
		rolloutsInformer,
		resyncPeriod,
		rolloutWorkqueue,
		serviceWorkqueue,
		metricsServer,
		recorder)
	serviceController := service.NewServiceController(
		kubeclientset,
		servicesInformer,
		rolloutsInformer,
		resyncPeriod,
		rolloutWorkqueue,
		serviceWorkqueue,
		metricsServer)

	cm := &Manager{
		metricsServer:     metricsServer,
		rolloutSynced:     rolloutsInformer.Informer().HasSynced,
		serviceSynced:     servicesInformer.Informer().HasSynced,
		replicasSetSynced: replicaSetInformer.Informer().HasSynced,
		rolloutWorkqueue:  rolloutWorkqueue,
		serviceWorkqueue:  serviceWorkqueue,
		rolloutController: rolloutController,
		serviceController: serviceController,
	}

	return cm
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Manager) Run(rolloutThreadiness, serviceThreadiness int, stopCh <-chan struct{}) error {

	defer runtime.HandleCrash()
	defer c.serviceWorkqueue.ShutDown()
	defer c.rolloutWorkqueue.ShutDown()
	// Wait for the caches to be synced before starting workers
	log.Info("Waiting for service controller's informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.serviceSynced, c.rolloutSynced, c.replicasSetSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// Start the informer factories to begin populating the informer caches
	log.Info("Starting Controllers")
	go wait.Until(func() { c.rolloutController.Run(rolloutThreadiness, stopCh) }, time.Second, stopCh)
	go wait.Until(func() { c.serviceController.Run(serviceThreadiness, stopCh) }, time.Second, stopCh)
	log.Info("Started controller")

	go func() {
		log.Infof("Starting Metric Server at %s", c.metricsServer.Addr)
		err := c.metricsServer.ListenAndServe()
		if err != nil {
			err = errors.Wrap(err, "Starting Metric Server")
			log.Fatal(err)
		}
	}()
	<-stopCh
	log.Info("Shutting down workers")

	return nil
}
