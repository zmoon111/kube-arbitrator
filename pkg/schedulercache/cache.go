/*
Copyright 2017 The Kubernetes Authors.

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

package schedulercache

import (
	"fmt"
	"sync"

	"github.com/golang/glog"
	apiv1 "github.com/kubernetes-incubator/kube-arbitrator/pkg/apis/v1"
	"github.com/kubernetes-incubator/kube-arbitrator/pkg/client"

	"k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	clientv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// New returns a Cache implementation.
func New(
	config *rest.Config,
) Cache {
	return newSchedulerCache(config)
}

type schedulerCache struct {
	sync.Mutex

	podInformer   clientv1.PodInformer
	nodeInformer  clientv1.NodeInformer
	rqaController cache.Controller

	pods                    map[string]*PodInfo
	nodes                   map[string]*NodeInfo
	resourceQuotaAllocators map[string]*ResourceQuotaAllocatorInfo
}

func newSchedulerCache(config *rest.Config) *schedulerCache {
	sc := &schedulerCache{
		nodes: make(map[string]*NodeInfo),
		pods:  make(map[string]*PodInfo),
		resourceQuotaAllocators: make(map[string]*ResourceQuotaAllocatorInfo),
	}

	kubecli := kubernetes.NewForConfigOrDie(config)
	informerFactory := informers.NewSharedInformerFactory(kubecli, 0)

	// create informer for node information
	sc.nodeInformer = informerFactory.Core().V1().Nodes()
	sc.nodeInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    sc.AddNode,
			UpdateFunc: sc.UpdateNode,
			DeleteFunc: sc.DeleteNode,
		},
		0,
	)

	// create informer for pod information
	sc.podInformer = informerFactory.Core().V1().Pods()
	sc.podInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					glog.V(4).Infof("filter pod name(%s) namespace(%s) status(%s)\n", t.Name, t.Namespace, t.Status.Phase)
					return true
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    sc.AddPod,
				UpdateFunc: sc.UpdatePod,
				DeleteFunc: sc.DeletePod,
			},
		})

	// create resourcequotaallocator resource first
	err := createResourceQuotaAllocatorCRD(config)
	if err != nil {
		panic(err)
	}
	// create informer/controller
	sc.rqaController, err = createResourceQuotaAllocatorCRDController(config, sc)
	if err != nil {
		panic(err)
	}

	return sc
}

func createResourceQuotaAllocatorCRD(config *rest.Config) error {
	extensionscs, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return err
	}
	_, err = client.CreateResourceQuotaAllocatorCRD(extensionscs)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func createResourceQuotaAllocatorCRDController(config *rest.Config, sc *schedulerCache) (cache.Controller, error) {
	rqaClient, _, err := client.NewClient(config)
	if err != nil {
		return nil, err
	}
	source := cache.NewListWatchFromClient(
		rqaClient,
		apiv1.ResourceQuotaAllocatorPlural,
		v1.NamespaceAll,
		fields.Everything())

	_, controller := cache.NewInformer(
		source,
		&apiv1.ResourceQuotaAllocator{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    sc.AddResourceQuotaAllocator,
			UpdateFunc: sc.UpdateResourceQuotaAllocator,
			DeleteFunc: sc.DeleteResourceQuotaAllocator,
		})

	return controller, nil
}

func (sc *schedulerCache) Run(stopCh <-chan struct{}) {
	go sc.podInformer.Informer().Run(stopCh)
	go sc.nodeInformer.Informer().Run(stopCh)
	go sc.rqaController.Run(stopCh)
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) addPod(pod *v1.Pod) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	if _, ok := sc.pods[key]; ok {
		return fmt.Errorf("pod %v exist", key)
	}

	info := &PodInfo{
		name: key,
		pod:  pod.DeepCopy(),
	}
	sc.pods[key] = info
	return nil
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) updatePod(oldPod, newPod *v1.Pod) error {
	if err := sc.deletePod(oldPod); err != nil {
		return err
	}
	sc.addPod(newPod)
	return nil
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) deletePod(pod *v1.Pod) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	if _, ok := sc.pods[key]; !ok {
		return fmt.Errorf("pod %v doesn't exist", key)
	}
	delete(sc.pods, key)
	return nil
}

func (sc *schedulerCache) AddPod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		glog.Errorf("cannot convert to *v1.Pod: %v", obj)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("ADD Pod(%s) into cache, status (%s)\n", pod.Name, pod.Status.Phase)
	err := sc.addPod(pod)
	if err != nil {
		glog.Errorf("failed to add pod %s into cache: %v", pod.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) UpdatePod(oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*v1.Pod)
	if !ok {
		glog.Errorf("cannot convert oldObj to *v1.Pod: %v", oldObj)
		return
	}
	newPod, ok := newObj.(*v1.Pod)
	if !ok {
		glog.Errorf("cannot convert newObj to *v1.Pod: %v", newObj)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("UPDATE oldPod(%s) status(%s) newPod(%s) status(%s) in cache\n", oldPod.Name, oldPod.Status.Phase, newPod.Name, newPod.Status.Phase)
	err := sc.updatePod(oldPod, newPod)
	if err != nil {
		glog.Errorf("failed to update pod %v in cache: %v", oldPod.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) DeletePod(obj interface{}) {
	var pod *v1.Pod
	switch t := obj.(type) {
	case *v1.Pod:
		pod = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = t.Obj.(*v1.Pod)
		if !ok {
			glog.Errorf("cannot convert to *v1.Pod: %v", t.Obj)
			return
		}
	default:
		glog.Errorf("cannot convert to *v1.Pod: %v", t)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("DELETE Pod(%s) status(%s) from cache\n", pod.Name, pod.Status.Phase)
	err := sc.deletePod(pod)
	if err != nil {
		glog.Errorf("failed to delete pod %v from cache: %v", pod.Name, err)
		return
	}
	return
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) addNode(node *v1.Node) error {
	if _, ok := sc.nodes[node.Name]; ok {
		return fmt.Errorf("node %v exist", node.Name)
	}

	info := &NodeInfo{
		name: node.Name,
		node: node.DeepCopy(),
	}
	sc.nodes[node.Name] = info
	return nil
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) updateNode(oldNode, newNode *v1.Node) error {
	if err := sc.deleteNode(oldNode); err != nil {
		return err
	}
	sc.addNode(newNode)
	return nil
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) deleteNode(node *v1.Node) error {
	if _, ok := sc.nodes[node.Name]; !ok {
		return fmt.Errorf("node %v doesn't exist", node.Name)
	}
	delete(sc.nodes, node.Name)
	return nil
}

func (sc *schedulerCache) AddNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		glog.Errorf("cannot convert to *v1.Node: %v", obj)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("ADD Node(%s) into cache\n", node.Name)
	err := sc.addNode(node)
	if err != nil {
		glog.Errorf("failed to add node %s into cache: %v", node.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) UpdateNode(oldObj, newObj interface{}) {
	oldNode, ok := oldObj.(*v1.Node)
	if !ok {
		glog.Errorf("cannot convert oldObj to *v1.Node: %v", oldObj)
		return
	}
	newNode, ok := newObj.(*v1.Node)
	if !ok {
		glog.Errorf("cannot convert newObj to *v1.Node: %v", newObj)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("UPDATE oldNode(%s) newNode(%s) in cache\n", oldNode.Name, newNode.Name)
	err := sc.updateNode(oldNode, newNode)
	if err != nil {
		glog.Errorf("failed to update node %v in cache: %v", oldNode.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) DeleteNode(obj interface{}) {
	var node *v1.Node
	switch t := obj.(type) {
	case *v1.Node:
		node = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		node, ok = t.Obj.(*v1.Node)
		if !ok {
			glog.Errorf("cannot convert to *v1.Node: %v", t.Obj)
			return
		}
	default:
		glog.Errorf("cannot convert to *v1.Node: %v", t)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("DELETE Node(%s) from cache\n", node.Name)
	err := sc.deleteNode(node)
	if err != nil {
		glog.Errorf("failed to delete node %s from cache: %v", node.Name, err)
		return
	}
	return
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) addResourceQuotaAllocator(rqa *apiv1.ResourceQuotaAllocator) error {
	if _, ok := sc.resourceQuotaAllocators[rqa.Name]; ok {
		return fmt.Errorf("resourceQuotaAllocator %v exist", rqa.Name)
	}

	info := &ResourceQuotaAllocatorInfo{
		name:      rqa.Name,
		allocator: rqa.DeepCopy(),
	}
	sc.resourceQuotaAllocators[rqa.Name] = info
	return nil
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) updateResourceQuotaAllocator(oldRqa, newRqa *apiv1.ResourceQuotaAllocator) error {
	if err := sc.deleteResourceQuotaAllocator(oldRqa); err != nil {
		return err
	}
	sc.addResourceQuotaAllocator(newRqa)
	return nil
}

// Assumes that lock is already acquired.
func (sc *schedulerCache) deleteResourceQuotaAllocator(rqa *apiv1.ResourceQuotaAllocator) error {
	if _, ok := sc.resourceQuotaAllocators[rqa.Name]; !ok {
		return fmt.Errorf("resourceQuotaAllocator %v doesn't exist", rqa.Name)
	}
	delete(sc.resourceQuotaAllocators, rqa.Name)
	return nil
}

func (sc *schedulerCache) AddResourceQuotaAllocator(obj interface{}) {
	rqa, ok := obj.(*apiv1.ResourceQuotaAllocator)
	if !ok {
		glog.Errorf("cannot convert to *apiv1.ResourceQuotaAllocator: %v", obj)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("ADD Allocator(%s) into cache, status(%#v), spec(%#v)\n", rqa.Name, rqa.Status, rqa.Spec)
	err := sc.addResourceQuotaAllocator(rqa)
	if err != nil {
		glog.Errorf("failed to add allocator %s into cache: %v", rqa.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) UpdateResourceQuotaAllocator(oldObj, newObj interface{}) {
	oldRqa, ok := oldObj.(*apiv1.ResourceQuotaAllocator)
	if !ok {
		glog.Errorf("cannot convert oldObj to *apiv1.ResourceQuotaAllocator: %v", oldObj)
		return
	}
	newRqa, ok := newObj.(*apiv1.ResourceQuotaAllocator)
	if !ok {
		glog.Errorf("cannot convert newObj to *apiv1.ResourceQuotaAllocator: %v", newObj)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	glog.V(4).Infof("UPDATE oldAllocator(%s) in cache, status(%#v), spec(%#v)\n", oldRqa.Name, oldRqa.Status, oldRqa.Spec)
	glog.V(4).Infof("UPDATE newAllocator(%s) in cache, status(%#v), spec(%#v)\n", newRqa.Name, newRqa.Status, newRqa.Spec)
	err := sc.updateResourceQuotaAllocator(oldRqa, newRqa)
	if err != nil {
		glog.Errorf("failed to update allocator %s into cache: %v", oldRqa.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) DeleteResourceQuotaAllocator(obj interface{}) {
	var rqa *apiv1.ResourceQuotaAllocator
	switch t := obj.(type) {
	case *apiv1.ResourceQuotaAllocator:
		rqa = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		rqa, ok = t.Obj.(*apiv1.ResourceQuotaAllocator)
		if !ok {
			glog.Errorf("cannot convert to *v1.Node: %v", t.Obj)
			return
		}
	default:
		glog.Errorf("cannot convert to *v1.Node: %v", t)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	err := sc.deleteResourceQuotaAllocator(rqa)
	if err != nil {
		glog.Errorf("failed to delete allocator %s from cache: %v", rqa.Name, err)
		return
	}
	return
}

func (sc *schedulerCache) Dump() *CacheSnapshot {
	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	snapshot := &CacheSnapshot{
		Nodes:      make([]*NodeInfo, 0, len(sc.nodes)),
		Pods:       make([]*PodInfo, 0, len(sc.pods)),
		Allocators: make([]*ResourceQuotaAllocatorInfo, 0, len(sc.resourceQuotaAllocators)),
	}

	for _, value := range sc.nodes {
		snapshot.Nodes = append(snapshot.Nodes, value.Clone())
	}
	for _, value := range sc.pods {
		snapshot.Pods = append(snapshot.Pods, value.Clone())
	}
	for _, value := range sc.resourceQuotaAllocators {
		snapshot.Allocators = append(snapshot.Allocators, value.Clone())
	}
	return snapshot
}
