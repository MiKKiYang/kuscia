// Copyright 2023 Ant Group Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package domain

import (
	"context"
	"fmt"
	"sync"
	"time"

	apicorev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apismetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	informerscorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	rbaclisters "k8s.io/client-go/listers/rbac/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/secretflow/kuscia/pkg/common"
	"github.com/secretflow/kuscia/pkg/controllers"
	"github.com/secretflow/kuscia/pkg/controllers/domain/metrics"
	kusciaapisv1alpha1 "github.com/secretflow/kuscia/pkg/crd/apis/kuscia/v1alpha1"
	kusciaclientset "github.com/secretflow/kuscia/pkg/crd/clientset/versioned"
	kusciainformers "github.com/secretflow/kuscia/pkg/crd/informers/externalversions"
	kusciaextv1alpha1 "github.com/secretflow/kuscia/pkg/crd/informers/externalversions/kuscia/v1alpha1"
	kuscialistersv1alpha1 "github.com/secretflow/kuscia/pkg/crd/listers/kuscia/v1alpha1"
	"github.com/secretflow/kuscia/pkg/utils/nlog"
	"github.com/secretflow/kuscia/pkg/utils/queue"
	"github.com/secretflow/kuscia/pkg/utils/resources"
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a object is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerName = "domain-controller"
)

const (
	resourceQuotaName = "resource-limitation"
	domainConfigName  = "domain-config"
)

const (
	addPod = "add"
	deletePod = "delete"
	addNode = "add"
	updateNode = "update"
	deleteNode = "delete"
)

const (
	nodeStatusReady    = "Ready"
	nodeStatusNotReady = "NotReady"
)

// Controller is the implementation for managing domain resources.
type Controller struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	RunMode               common.RunModeType
	Namespace             string
	RootDir               string
	kubeClient            kubernetes.Interface
	kusciaClient          kusciaclientset.Interface
	kubeInformerFactory   kubeinformers.SharedInformerFactory
	kusciaInformerFactory kusciainformers.SharedInformerFactory
	resourceQuotaLister   listerscorev1.ResourceQuotaLister
	domainLister          kuscialistersv1alpha1.DomainLister
	namespaceLister       listerscorev1.NamespaceLister
	nodeLister            listerscorev1.NodeLister
	configmapLister       listerscorev1.ConfigMapLister
	roleLister            rbaclisters.RoleLister
	podLister			  listerscorev1.PodLister
	workqueue             workqueue.RateLimitingInterface
	podQueue              workqueue.RateLimitingInterface
	nodeQueue	          workqueue.RateLimitingInterface
	recorder              record.EventRecorder
	cacheSyncs            []cache.InformerSynced
	nodeStatusManager 	  *common.NodeStatusManager
	nodeStatusesLock      sync.RWMutex
}

// NewController returns a controller instance.
func NewController(ctx context.Context, config controllers.ControllerConfig) controllers.IController {
	kubeClient := config.KubeClient
	kusciaClient := config.KusciaClient
	eventRecorder := config.EventRecorder
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, 5*time.Minute)
	resourceQuotaInformer := kubeInformerFactory.Core().V1().ResourceQuotas()
	namespaceInformer := kubeInformerFactory.Core().V1().Namespaces()
	nodeInformer := kubeInformerFactory.Core().V1().Nodes()
	podInformer := kubeInformerFactory.Core().V1().Pods()
	configmapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
	roleInformer := kubeInformerFactory.Rbac().V1().Roles()

	kusciaInformerFactory := kusciainformers.NewSharedInformerFactory(kusciaClient, 5*time.Minute)
	domainInformer := kusciaInformerFactory.Kuscia().V1alpha1().Domains()

	cacheSyncs := []cache.InformerSynced{
		resourceQuotaInformer.Informer().HasSynced,
		domainInformer.Informer().HasSynced,
		namespaceInformer.Informer().HasSynced,
		nodeInformer.Informer().HasSynced,
		configmapInformer.Informer().HasSynced,
		roleInformer.Informer().HasSynced,
	}
	controller := &Controller{
		RunMode:               config.RunMode,
		Namespace:             config.Namespace,
		RootDir:               config.RootDir,
		kubeClient:            kubeClient,
		kusciaClient:          kusciaClient,
		kubeInformerFactory:   kubeInformerFactory,
		kusciaInformerFactory: kusciaInformerFactory,
		resourceQuotaLister:   resourceQuotaInformer.Lister(),
		domainLister:          domainInformer.Lister(),
		namespaceLister:       namespaceInformer.Lister(),
		nodeLister:            nodeInformer.Lister(),
		podLister:             podInformer.Lister(),
		configmapLister:       configmapInformer.Lister(),
		roleLister:            roleInformer.Lister(),
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "domain"),
		podQueue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "pod"),
		nodeQueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "node"),
		recorder:              eventRecorder,
		cacheSyncs:            cacheSyncs,
		nodeStatusManager:     common.NewNodeStatusManager(),
	}

	controller.ctx, controller.cancel = context.WithCancel(ctx)

	controller.addNamespaceEventHandler(namespaceInformer)
	controller.addDomainEventHandler(domainInformer)
	controller.addResourceQuotaEventHandler(resourceQuotaInformer)
	controller.addConfigMapHandler(configmapInformer)
	controller.addPodEventHandler(podInformer)
	controller.addNodeEventHandler(nodeInformer)

	return controller
}

func (c *Controller) addPodEventHandler(podInformer informerscorev1.PodInformer) {
	_, _ = podInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			pod, ok := obj.(*apicorev1.Pod)
			if ok {
				namespace := pod.Namespace
				nodeName := pod.Spec.NodeName
				_, err := c.domainLister.Get(namespace)
				if err != nil {
					nlog.Errorf("domainLister get %s failed with %v", namespace, err)
					return false
				}

				if nodeName == "" {
					nlog.Errorf("pod %v not hv nodeName", pod)
					return false
				}
				return true
			}
			nlog.Errorf("item %v is not pod type with %v", obj, ok)
			return false
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handlePodAdd,
			DeleteFunc: c.handlePodDelete,
		},
	})
}

func (c *Controller) addNodeEventHandler(nodeInformer informerscorev1.NodeInformer) {
	_, _ = nodeInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			nodeObj, ok := obj.(*apicorev1.Node)
			if ok {
				if c.matchNodeLabels(nodeObj) {
					return true
				}
			}
			return false
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: c.handleNodeAdd,
			UpdateFunc: c.handleNodeUpdate,
			DeleteFunc: c.handleNodeDelete,
		},
	})
}

func (c *Controller) handleNodeAdd(obj interface{}) {
	c.handleNodeCommon(obj, addNode)
}

func (c *Controller) handleNodeUpdate(_, newObj interface{}) {
	c.handleNodeCommon(newObj, updateNode)
}

func (c *Controller) handleNodeDelete(obj interface{}) {
	c.handleNodeCommon(obj, deleteNode)
}

func (c *Controller) handleNodeCommon(obj interface{}, op string) {
	newNode, ok := obj.(*apicorev1.Node)
	if !ok {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			if newNode, ok = d.Obj.(*apicorev1.Node); !ok {
				nlog.Warnf("Could not convert object %T to Node", d.Obj)
				return
			}
		} else {
			nlog.Warnf("Received unexpected object type %T for node %s event", obj, op)
			return
		}
	}

	queue.EnqueueNodeObject(&queue.NodeQueueItem{Node: newNode}, c.nodeQueue)
}

func (c *Controller) handlePodAdd(obj interface{}) {
	c.handlePodCommon(obj, addPod)
}

func (c *Controller) handlePodDelete(obj interface{}) {
	c.handlePodCommon(obj, deletePod)
}

func (c *Controller) handlePodCommon(obj interface{}, op string)  {
	Pod, ok := obj.(*apicorev1.Pod)
	if !ok {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			if Pod, ok = d.Obj.(*apicorev1.Pod); !ok {
				nlog.Warnf("Could not convert object %T to Pod", d.Obj)
				return
			}
		} else {
			nlog.Warnf("Received unexpected object type %T for pod %s event", obj, op)
			return
		}
	}

	queue.EnqueuePodObject(&queue.PodQueueItem{Pod: Pod, Op: op}, c.podQueue)
}

func (c *Controller) nodeHandler(item *queue.NodeQueueItem) error {
	newStatus := common.LocalNodeStatus{
		Name:              item.Node.Name,
		DomainName:        item.Node.Labels[common.LabelNodeNamespace],
	}

	for _, cond := range item.Node.Status.Conditions {
		if cond.Type == apicorev1.NodeReady {
			switch cond.Status {
			case apicorev1.ConditionTrue:
				newStatus.Status = nodeStatusReady
			default:
				newStatus.Status = nodeStatusNotReady
				for _, condReason := range item.Node.Status.Conditions {
					if condReason.Status == apicorev1.ConditionTrue {
						newStatus.UnreadyReason = string(condReason.Type)
					}
					break
				}
			}
			newStatus.LastHeartbeatTime = cond.LastHeartbeatTime
			newStatus.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}
	return c.nodeStatusManager.UpdateStatus(newStatus)
}

func (c *Controller) podHandler(item *queue.PodQueueItem) error {
	switch item.Op {
	case addPod:
		return c.addPodHandler(item.Pod)
	case deletePod:
		return c.deletePodHandler(item.Pod)
	default:
		return fmt.Errorf("unknown operation: %s", item.Op)
	}
}

func (c *Controller) addPodHandler(pod *apicorev1.Pod) error {
	cpuReq, memReq := c.calRequestResource(pod)
	return c.nodeStatusManager.AddPodResources(pod.Spec.NodeName, cpuReq, memReq)
}

func (c *Controller) deletePodHandler(pod *apicorev1.Pod) error {
	cpuReq, memReq := c.calRequestResource(pod)
	return c.nodeStatusManager.RemovePodResources(pod.Spec.NodeName, cpuReq, memReq)
}

func (c * Controller) calRequestResource(pod *apicorev1.Pod) (int64, int64) {
	var requestCPURequest, requestMEMRequest int64
	for _, container := range pod.Spec.Containers {
		requestCPURequest += container.Resources.Requests.Cpu().MilliValue()
		requestMEMRequest += container.Resources.Requests.Memory().Value()
	}

	return requestCPURequest, requestMEMRequest
}

// addNamespaceEventHandler is used to add event handler for namespace informer.
func (c *Controller) addNamespaceEventHandler(nsInformer informerscorev1.NamespaceInformer) {
	_, _ = nsInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *apicorev1.Namespace:
				return c.matchLabels(t)
			case cache.DeletedFinalStateUnknown:
				if rq, ok := t.Obj.(*apicorev1.Namespace); ok {
					return c.matchLabels(rq)
				}
				return false
			default:
				return false
			}
		},

		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: c.enqueueNamespace,
			UpdateFunc: func(oldObj, newObj interface{}) {
				newNs, ok := newObj.(*apicorev1.Namespace)
				if !ok {
					nlog.Warnf("Unable convert object %T to Namespace", newNs)
					return
				}
				oldNs, ok := oldObj.(*apicorev1.Namespace)
				if !ok {
					nlog.Warnf("Unable convert object %T to Namespace", oldNs)
					return
				}

				if newNs.ResourceVersion == oldNs.ResourceVersion {
					return
				}
				c.enqueueNamespace(newObj)
			},
			DeleteFunc: c.enqueueNamespace,
		},
	})
}

// addDomainEventHandler is used to add event handler for domain informer.
func (c *Controller) addDomainEventHandler(domainInformer kusciaextv1alpha1.DomainInformer) {
	_, _ = domainInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueDomain,
		UpdateFunc: func(oldObj, newObj interface{}) {
			newDomain, ok := newObj.(*kusciaapisv1alpha1.Domain)
			if !ok {
				nlog.Error("Unable convert object to domain")
				return
			}
			oldDomain, ok := oldObj.(*kusciaapisv1alpha1.Domain)
			if !ok {
				nlog.Error("Unable convert object to domain")
				return
			}

			if newDomain.ResourceVersion == oldDomain.ResourceVersion {
				return
			}
			c.enqueueDomain(newObj)
		},
		DeleteFunc: c.enqueueDomain,
	})
}

// addResourceQuotaEventHandler is used to add event handler for resource quota informer.
func (c *Controller) addResourceQuotaEventHandler(rqInformer informerscorev1.ResourceQuotaInformer) {
	_, _ = rqInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *apicorev1.ResourceQuota:
				return c.matchLabels(t)
			case cache.DeletedFinalStateUnknown:
				if rq, ok := t.Obj.(*apicorev1.ResourceQuota); ok {
					return c.matchLabels(rq)
				}
				return false
			default:
				return false
			}
		},

		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: c.enqueueResourceQuota,
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldRQ, ok := oldObj.(*apicorev1.ResourceQuota)
				if !ok {
					nlog.Error("Unable convert object to resource quota")
					return
				}
				newRQ, ok := newObj.(*apicorev1.ResourceQuota)
				if !ok {
					nlog.Error("Unable convert object to resource quota")
					return
				}
				if oldRQ.ResourceVersion == newRQ.ResourceVersion {
					return
				}
				c.enqueueResourceQuota(newObj)
			},
			DeleteFunc: c.enqueueResourceQuota,
		},
	})
}

// addConfigMapHandler is used to add event handler for configmap informer.
func (c *Controller) addConfigMapHandler(cmInformer informerscorev1.ConfigMapInformer) {
	_, _ = cmInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *apicorev1.ConfigMap:
				return c.matchLabels(t)
			case cache.DeletedFinalStateUnknown:
				if cm, ok := t.Obj.(*apicorev1.ConfigMap); ok {
					return c.matchLabels(cm)
				}
				return false
			default:
				return false
			}
		},

		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueConfigMap,
			DeleteFunc: c.enqueueConfigMap,
		},
	})
}

func (c *Controller) matchNodeLabels(obj *apicorev1.Node) bool {
	if objLabels := obj.GetLabels(); objLabels != nil {
		if value, exists := objLabels[common.LabelNodeNamespace]; exists {
			if value != "" {
				_, err := c.domainLister.Get(value)
				if err != nil {
					nlog.Errorf("get domain by node %s failed with %v", obj.Name, err)
					return false
				}
				return true
			}
			nlog.Errorf("node %s hv no domain belonged to", obj.Name)
		}
		nlog.Errorf("node %s hv no label about domain", obj.Name)
	}
	nlog.Errorf("node %s get labels failed", obj.Name)
	return false
}

// matchLabels is used to filter concerned resource.
func (c *Controller) matchLabels(obj apismetav1.Object) bool {
	if labels := obj.GetLabels(); labels != nil {
		_, ok := labels[common.LabelDomainName]
		if ok {
			return true
		}
	}
	return false
}

// enqueueDomain puts a domain resource onto the workqueue.
// This method should *not* be passed resources of any type other than domain.
func (c *Controller) enqueueDomain(obj interface{}) {
	queue.EnqueueObjectWithKey(obj, c.workqueue)
}

// enqueueResourceQuota puts a resource quota resource onto the workqueue.
// This method should *not* be passed resources of any type other than resource quota.
func (c *Controller) enqueueResourceQuota(obj interface{}) {
	queue.EnqueueObjectWithKeyNamespace(obj, c.workqueue)
}

// enqueueNamespace puts a namespace resource onto the workqueue.
// This method should *not* be passed resources of any type other than namespace.
func (c *Controller) enqueueNamespace(obj interface{}) {
	queue.EnqueueObjectWithKey(obj, c.workqueue)
}

// enqueueConfigMap puts a configmap resource onto the workqueue.
// This method should *not* be passed resources of any type other than resource quota.
func (c *Controller) enqueueConfigMap(obj interface{}) {
	queue.EnqueueObjectWithKeyNamespace(obj, c.workqueue)
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(workers int) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	nlog.Info("Starting domain controller")
	c.kusciaInformerFactory.Start(c.ctx.Done())
	c.kubeInformerFactory.Start(c.ctx.Done())

	nlog.Info("Waiting for informer cache to sync")
	if !cache.WaitForCacheSync(c.ctx.Done(), c.cacheSyncs...) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	nlog.Infof("Starting Init LocalNodeStatus")
	err := c.initLocalNodeStatus()
	if err != nil {
		return fmt.Errorf("failed to initLocalNodeStatus")
	}

	nlog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, c.ctx.Done())
		go wait.Until(c.runPodHandleWorker, time.Second, c.ctx.Done())
		go wait.Until(c.runNodeHandleWorker, time.Second, c.ctx.Done())
	}

	nlog.Info("Starting sync domain status")
	go wait.Until(c.syncDomainStatuses, 10*time.Second, c.ctx.Done())
	<-c.ctx.Done()
	return nil
}

func (c *Controller) initLocalNodeStatus() error {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("domain controller init localNodeStatus failed with %v", err)
	}

	c.nodeStatusesLock.Lock()
    defer c.nodeStatusesLock.Unlock()

	var nodeStatuses []common.LocalNodeStatus
	for _, nodeObj := range nodes {
		if !c.matchNodeLabels(nodeObj) {
			continue
		}

		domainName := nodeObj.Labels[common.LabelNodeNamespace]
        
        var totalCPU, totalMem int64
        pods, _ := c.podLister.Pods(domainName).List(labels.Everything())
        for _, pod := range pods {
            if pod.Spec.NodeName == nodeObj.Name {
                cpu, mem := c.calRequestResource(pod)
                totalCPU += cpu
                totalMem += mem
            }
        }

		status := common.LocalNodeStatus{
            Name:          nodeObj.Name,
            DomainName:    domainName,
            TotalCPURequest: totalCPU,
            TotalMemRequest: totalMem,
            Status:         nodeStatusNotReady,
            LastHeartbeatTime:  nodeObj.Status.Conditions[0].LastHeartbeatTime,
            LastTransitionTime: nodeObj.Status.Conditions[0].LastTransitionTime,
        }

		for _, cond := range nodeObj.Status.Conditions {
            if cond.Type == apicorev1.NodeReady {
                if cond.Status == apicorev1.ConditionTrue {
                    status.Status = nodeStatusReady
                }
                status.LastHeartbeatTime = cond.LastHeartbeatTime
                status.LastTransitionTime = cond.LastTransitionTime
                break
            }
        }

		nodeStatuses = append(nodeStatuses, status)
	}

	c.nodeStatusManager.ReplaceAll(nodeStatuses)

	for i, status := range c.nodeStatusManager.GetAll() {
		nlog.Debugf("NodeStatus[%d]:\n"+
			"Name: %s\n"+
			"Domain: %s\n"+
			"Status: %s\n"+
			"LastHeartbeatTime: %s\n" +
			"LastTransitionTime: %s\n" +
			"UnreadyReason: %s\n"+
			"CPU: %d\n"+
			"Memory: %d",
			i, status.Name, status.DomainName, status.Status,
			status.LastHeartbeatTime.Format(time.RFC3339), status.LastTransitionTime.Format(time.RFC3339),
			status.UnreadyReason, status.TotalCPURequest, status.TotalMemRequest)
	}
	return nil
}

// Stop is used to stop the controller.
func (c *Controller) Stop() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the workqueue.
func (c *Controller) runWorker() {
	for queue.HandleQueueItem(context.Background(), controllerName, c.workqueue, c.syncHandler, maxRetries) {
		metrics.WorkerQueueSize.Set(float64(c.workqueue.Len()))
	}
}

func (c *Controller) runPodHandleWorker() {
	for queue.HandlePodQueueItem(context.Background(), controllerName, c.podQueue, c.podHandler, maxRetries) {
		metrics.WorkerQueueSize.Set(float64(c.podQueue.Len()))
	}
}

func (c *Controller) runNodeHandleWorker() {
	for queue.HandleNodeQueueItem(context.Background(), controllerName, c.nodeQueue, c.nodeHandler, maxRetries) {
		metrics.WorkerQueueSize.Set(float64(c.nodeQueue.Len()))
	}
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the domain resource
// with the current status of the resource.
func (c *Controller) syncHandler(ctx context.Context, key string) (err error) {
	rawDomain, err := c.domainLister.Get(key)
	if err != nil {
		// domain resource is deleted
		if k8serrors.IsNotFound(err) {
			return c.delete(key)
		}
		return err
	}

	domain := rawDomain.DeepCopy()
	scheme.Scheme.Default(domain)

	if _, err = c.namespaceLister.Get(key); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.create(domain)
		}
		return err
	}

	return c.update(domain)
}

// create is used to create resource under domain.
func (c *Controller) create(domain *kusciaapisv1alpha1.Domain) error {
	if err := c.createNamespace(domain); err != nil {
		nlog.Warnf("Create domain %v namespace failed: %v", domain.Name, err.Error())
		return err
	}

	if err := c.createOrUpdateDomainRole(domain); err != nil {
		nlog.Warnf("Create or update domain %v role failed: %v", domain.Name, err.Error())
		return err
	}

	if !isPartner(domain) {
		if err := c.createDomainConfig(domain); err != nil {
			nlog.Warnf("Create domain %v configmap failed: %v", domain.Name, err.Error())
			return err
		}

		if err := c.createResourceQuota(domain); err != nil {
			nlog.Warnf("Create domain %v resource quota failed: %v", domain.Name, err.Error())
			return err
		}
	}

	if shouldCreateOrUpdate(domain) {
		if err := c.createOrUpdateAuth(domain); err != nil {
			nlog.Warnf("Create domain %v auth failed: %v", domain.Name, err.Error())
			return err
		}
	}

	return nil
}

// update is used to update resource under domain.
func (c *Controller) update(domain *kusciaapisv1alpha1.Domain) error {
	if err := c.updateNamespace(domain); err != nil {
		nlog.Warnf("Update domain %v namespace failed: %v", domain.Name, err.Error())
		return err
	}

	if err := c.createOrUpdateDomainRole(domain); err != nil {
		nlog.Warnf("Create or update domain %v role failed: %v", domain.Name, err.Error())
		return err
	}

	if shouldCreateOrUpdate(domain) {
		if err := c.createOrUpdateAuth(domain); err != nil {
			nlog.Warnf("update domain %v auth failed: %v", domain.Name, err.Error())
			return err
		}
		return nil
	}

	if !isPartner(domain) {
		if err := c.createDomainConfig(domain); err != nil {
			nlog.Warnf("Create domain %v configmap failed: %v", domain.Name, err.Error())
			return err
		}

		if err := c.updateResourceQuota(domain); err != nil {
			nlog.Warnf("Update domain %v resource quota failed: %v", domain.Name, err.Error())
			return err
		}

		if err := c.syncDomainStatus(domain); err != nil {
			nlog.Warnf("sync domain %v status failed: %v", domain.Name, err.Error())
			return err
		}
	}
	return nil
}

func (c *Controller) createOrUpdateDomainRole(domain *kusciaapisv1alpha1.Domain) error {
	ownerRef := apismetav1.NewControllerRef(domain, kusciaapisv1alpha1.SchemeGroupVersion.WithKind("Domain"))
	return resources.CreateOrUpdateRole(context.Background(), c.kubeClient, c.roleLister, c.RootDir, domain.Name, ownerRef)
}

// delete is used to delete resource under domain.
func (c *Controller) delete(name string) error {
	if err := c.deleteNamespace(name); err != nil {
		nlog.Errorf("Delete domain %v namespace failed: %v", name, err.Error())
		return err
	}

	return nil
}

func (c *Controller) Name() string {
	return controllerName
}

func isPartner(domain *kusciaapisv1alpha1.Domain) bool {
	if domain.Spec.Role == kusciaapisv1alpha1.Partner {
		return true
	}
	return false
}
