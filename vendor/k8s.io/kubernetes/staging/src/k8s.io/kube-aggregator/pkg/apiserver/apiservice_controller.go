/*
Copyright 2016 The Kubernetes Authors.

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

package apiserver

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/api"
	internalinformers "k8s.io/kubernetes/pkg/client/informers/informers_generated/internalversion/core/internalversion"
	internallisters "k8s.io/kubernetes/pkg/client/listers/core/internalversion"

	"k8s.io/kube-aggregator/pkg/apis/apiregistration"
	informers "k8s.io/kube-aggregator/pkg/client/informers/internalversion/apiregistration/internalversion"
	listers "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/internalversion"
	"k8s.io/kube-aggregator/pkg/controllers"
)

type APIHandlerManager interface {
	AddAPIService(apiService *apiregistration.APIService, destinationHost string)
	RemoveAPIService(apiServiceName string)
}

type APIServiceRegistrationController struct {
	apiHandlerManager APIHandlerManager

	apiServiceLister listers.APIServiceLister
	apiServiceSynced cache.InformerSynced

	// serviceLister is used to get the IP to create the transport for
	serviceLister  internallisters.ServiceLister
	servicesSynced cache.InformerSynced

	// To allow injection for testing.
	syncFn func(key string) error

	queue workqueue.RateLimitingInterface
}

func NewAPIServiceRegistrationController(apiServiceInformer informers.APIServiceInformer, serviceInformer internalinformers.ServiceInformer, apiHandlerManager APIHandlerManager) *APIServiceRegistrationController {
	c := &APIServiceRegistrationController{
		apiHandlerManager: apiHandlerManager,
		apiServiceLister:  apiServiceInformer.Lister(),
		apiServiceSynced:  apiServiceInformer.Informer().HasSynced,
		serviceLister:     serviceInformer.Lister(),
		servicesSynced:    serviceInformer.Informer().HasSynced,
		queue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "APIServiceRegistrationController"),
	}

	apiServiceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addAPIService,
		UpdateFunc: c.updateAPIService,
		DeleteFunc: c.deleteAPIService,
	})

	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addService,
		UpdateFunc: c.updateService,
		DeleteFunc: c.deleteService,
	})

	c.syncFn = c.sync

	return c
}

func (c *APIServiceRegistrationController) sync(key string) error {
	apiService, err := c.apiServiceLister.Get(key)
	if apierrors.IsNotFound(err) {
		c.apiHandlerManager.RemoveAPIService(key)
		return nil
	}
	if err != nil {
		return err
	}

	// remove registration handling for APIServices which are not available
	if !apiregistration.IsAPIServiceConditionTrue(apiService, apiregistration.Available) {
		c.apiHandlerManager.RemoveAPIService(key)
		return nil
	}

	// TODO move the destination host to status so that you can see where its going
	c.apiHandlerManager.AddAPIService(apiService, c.getDestinationHost(apiService))
	return nil
}

func (c *APIServiceRegistrationController) getDestinationHost(apiService *apiregistration.APIService) string {
	if apiService.Spec.Service == nil {
		return ""
	}

	destinationHost := apiService.Spec.Service.Name + "." + apiService.Spec.Service.Namespace + ".svc"
	service, err := c.serviceLister.Services(apiService.Spec.Service.Namespace).Get(apiService.Spec.Service.Name)
	if err != nil {
		return destinationHost
	}
	switch {
	// use IP from a clusterIP for these service types
	case service.Spec.Type == api.ServiceTypeClusterIP,
		service.Spec.Type == api.ServiceTypeNodePort,
		service.Spec.Type == api.ServiceTypeLoadBalancer:
		return service.Spec.ClusterIP
	}

	// return the normal DNS name by default
	return destinationHost
}

func (c *APIServiceRegistrationController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting APIServiceRegistrationController")
	defer glog.Infof("Shutting down APIServiceRegistrationController")

	if !controllers.WaitForCacheSync("APIServiceRegistrationController", stopCh, c.apiServiceSynced, c.servicesSynced) {
		return
	}

	// only start one worker thread since its a slow moving API and the aggregation server adding bits
	// aren't threadsafe
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *APIServiceRegistrationController) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (c *APIServiceRegistrationController) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncFn(key.(string))
	if err == nil {
		c.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))
	c.queue.AddRateLimited(key)

	return true
}

func (c *APIServiceRegistrationController) enqueue(obj *apiregistration.APIService) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %#v: %v", obj, err)
		return
	}

	c.queue.Add(key)
}

func (c *APIServiceRegistrationController) addAPIService(obj interface{}) {
	castObj := obj.(*apiregistration.APIService)
	glog.V(4).Infof("Adding %s", castObj.Name)
	c.enqueue(castObj)
}

func (c *APIServiceRegistrationController) updateAPIService(obj, _ interface{}) {
	castObj := obj.(*apiregistration.APIService)
	glog.V(4).Infof("Updating %s", castObj.Name)
	c.enqueue(castObj)
}

func (c *APIServiceRegistrationController) deleteAPIService(obj interface{}) {
	castObj, ok := obj.(*apiregistration.APIService)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %#v", obj)
			return
		}
		castObj, ok = tombstone.Obj.(*apiregistration.APIService)
		if !ok {
			glog.Errorf("Tombstone contained object that is not expected %#v", obj)
			return
		}
	}
	glog.V(4).Infof("Deleting %q", castObj.Name)
	c.enqueue(castObj)
}

// there aren't very many apiservices, just check them all.
func (c *APIServiceRegistrationController) getAPIServicesFor(service *api.Service) []*apiregistration.APIService {
	var ret []*apiregistration.APIService
	apiServiceList, _ := c.apiServiceLister.List(labels.Everything())
	for _, apiService := range apiServiceList {
		if apiService.Spec.Service == nil {
			continue
		}
		if apiService.Spec.Service.Namespace == service.Namespace && apiService.Spec.Service.Name == service.Name {
			ret = append(ret, apiService)
		}
	}

	return ret
}

// TODO, think of a way to avoid checking on every service manipulation

func (c *APIServiceRegistrationController) addService(obj interface{}) {
	for _, apiService := range c.getAPIServicesFor(obj.(*api.Service)) {
		c.enqueue(apiService)
	}
}

func (c *APIServiceRegistrationController) updateService(obj, _ interface{}) {
	for _, apiService := range c.getAPIServicesFor(obj.(*api.Service)) {
		c.enqueue(apiService)
	}
}

func (c *APIServiceRegistrationController) deleteService(obj interface{}) {
	castObj, ok := obj.(*api.Service)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %#v", obj)
			return
		}
		castObj, ok = tombstone.Obj.(*api.Service)
		if !ok {
			glog.Errorf("Tombstone contained object that is not expected %#v", obj)
			return
		}
	}
	for _, apiService := range c.getAPIServicesFor(castObj) {
		c.enqueue(apiService)
	}
}
