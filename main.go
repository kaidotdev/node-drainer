package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"k8s.io/client-go/rest"

	"k8s.io/kubectl/pkg/drain"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	v1beta1Informers "k8s.io/client-go/informers/events/v1beta1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	v1beta1Listers "k8s.io/client-go/listers/events/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubectl/pkg/scheme"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/klog"
)

const (
	controllerAgentName    = "node-drainer"
	inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

type Controller struct {
	clientset    kubernetes.Interface
	lister       v1beta1Listers.EventLister
	synced       cache.InformerSynced
	workqueue    workqueue.RateLimitingInterface
	recorder     record.EventRecorder
	targetEvents []string
}

func NewController(clientset kubernetes.Interface, informer v1beta1Informers.EventInformer, targetEvents []string) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		clientset:    clientset,
		lister:       informer.Lister(),
		synced:       informer.Informer().HasSynced,
		workqueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Events"),
		recorder:     recorder,
		targetEvents: targetEvents,
	}

	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(object interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(object)
			if err != nil {
				utilruntime.HandleError(err)
				return
			}
			controller.workqueue.Add(key)
		},
	})

	return controller
}

func (c *Controller) Run(concurrency int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.synced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	for i := 0; i < concurrency; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh

	return nil
}

func (c *Controller) runWorker() {
	for {
		object, shutdown := c.workqueue.Get()

		if shutdown {
			return
		}

		err := func(object interface{}) error {
			defer c.workqueue.Done(object)
			key, ok := object.(string)
			if !ok {
				c.workqueue.Forget(object)
				return fmt.Errorf("expected string in workqueue but got %#v", object)
			}
			if err := c.syncHandler(key); err != nil {
				c.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
			c.workqueue.Forget(object)
			return nil
		}(object)

		if err != nil {
			utilruntime.HandleError(err)
		}
	}
}

func (c *Controller) isTarget(reason string) bool {
	for _, e := range c.targetEvents {
		if e == reason {
			return true
		}
	}
	return false
}

func (c *Controller) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	event, err := c.lister.Events(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("event '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	if c.isTarget(event.Reason) {
		helper := &drain.Helper{
			Client:              c.clientset,
			Force:               true,
			GracePeriodSeconds:  -1,
			Out:                 os.Stdout,
			ErrOut:              os.Stderr,
			IgnoreAllDaemonSets: true,
			DeleteLocalData:     true,
		}

		var options metav1.GetOptions
		nodeName := event.Regarding.Name

		node, err := c.clientset.CoreV1().Nodes().Get(nodeName, options)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil
			}

			return err
		}

		if err := drain.RunCordonOrUncordon(helper, node, true); err != nil {
			return fmt.Errorf("error cordoning node: %v", err)
		}

		if err := drain.RunNodeDrain(helper, nodeName); err != nil {
			return fmt.Errorf("error draining node: %v", err)
		}
	}

	return nil
}

func main() {
	var concurrency int
	var targetEvents string
	flag.IntVar(&concurrency, "concurrency", 1, "Concurrency of worker")
	flag.StringVar(&targetEvents, "target-events", "ContainerGCFailed,ImageGCFailed", "List of events reason to be drained the node")
	flag.Parse()
	klog.InitFlags(nil)

	stopCh := make(chan struct{})

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM)
	go func() {
		<-quit
		close(stopCh)
	}()

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("could not create kubernetes config: %s\n", err.Error())
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		klog.Fatalf("could not create kubernetes client: %s\n", err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id := uuid.New().String()
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name: controllerAgentName,
			Namespace: func() string {
				namespace, err := ioutil.ReadFile(inClusterNamespacePath)
				if err != nil {
					klog.Fatalf("unable to find leader election namespace: %v", err)
				}
				return string(namespace)
			}(),
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				informerFactory := informers.NewSharedInformerFactory(clientset, time.Second*10)
				controller := NewController(clientset, informerFactory.Events().V1beta1().Events(), strings.Split(targetEvents, ","))
				informerFactory.Start(stopCh)

				if err := controller.Run(concurrency, stopCh); err != nil {
					klog.Fatalf("Error running controller: %s", err.Error())
				}
			},
			OnStoppedLeading: func() {
				klog.Infof("leader lost: %s", id)
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					return
				}
				klog.Infof("new leader elected: %s", identity)
			},
		},
	})
}
