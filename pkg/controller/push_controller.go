package controller

import (
	"fmt"
	"time"

	pushapi "github.com/caicloud/kubepush/pkg/api"
	"github.com/caicloud/kubepush/pkg/dockerclient"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/workqueue"

	"k8s.io/client-go/1.4/dynamic"
	"k8s.io/client-go/1.4/kubernetes"
	"k8s.io/client-go/1.4/pkg/api"
	"k8s.io/client-go/1.4/pkg/api/unversioned"
	"k8s.io/client-go/1.4/pkg/api/v1"
	"k8s.io/client-go/1.4/pkg/labels"
	"k8s.io/client-go/1.4/pkg/runtime"
	"k8s.io/client-go/1.4/pkg/util/json"
	"k8s.io/client-go/1.4/pkg/util/wait"
	"k8s.io/client-go/1.4/pkg/watch"
	"k8s.io/client-go/1.4/tools/cache"
)

const (
	defaultLabelKey       = "kubepush.alpha.kubernetes.io/nodename"
	usernameAnnotationKey = "kubepush.alpha.kubernetes.io/username"
	passwordAnnotationKey = "kubepush.alpha.kubernetes.io/password"
)

var keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

type PushController struct {
	dynamicClient *dynamic.Client
	clientset     *kubernetes.Clientset
	dockerClient  dockerclient.DockerInterface

	pushController *cache.Controller
	pushStore      cache.Store
	queue          *workqueue.Type

	syncHandler func(rsKey string) error

	watchNamespace string
	watchNode      string
}

func NewPushController(clientset *kubernetes.Clientset, dynamicClient *dynamic.Client, dockerClient dockerclient.DockerInterface, namespace, node string, resyncPeriod time.Duration) *PushController {
	pc := &PushController{
		clientset:     clientset,
		dynamicClient: dynamicClient,
		dockerClient:  dockerClient,

		watchNamespace: namespace,
		watchNode:      node,
		queue:          workqueue.NewNamed("pushagent"),
	}

	opts := &api.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set(map[string]string{defaultLabelKey: pc.watchNode}))}

	pc.pushStore, pc.pushController = cache.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return pc.dynamicClient.Resource(&unversioned.APIResource{Name: "pushs", Namespaced: true, Kind: "push"}, pc.watchNamespace).List(opts)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return pc.dynamicClient.Resource(&unversioned.APIResource{Name: "pushs", Namespaced: true, Kind: "push"}, pc.watchNamespace).Watch(opts)
			},
		},
		&runtime.Unstructured{},
		resyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc: pc.enqueuePush,
			UpdateFunc: func(old, cur interface{}) {
				pc.enqueuePush(cur)
			},
			DeleteFunc: pc.enqueuePush,
		},
	)

	pc.syncHandler = pc.syncPush

	return pc
}

func (pc *PushController) enqueuePush(obj interface{}) {
	key, err := keyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	pc.queue.Add(key)
}

func (pc *PushController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go pc.pushController.Run(stopCh)
	for i := 0; i < workers; i++ {
		go wait.Until(pc.worker, time.Second, stopCh)
	}
	<-stopCh
	glog.Infof("Shutting down Push Controller")
	pc.queue.ShutDown()
}

func (pc *PushController) worker() {
	for {
		func() {
			key, quit := pc.queue.Get()
			if quit {
				return
			}
			defer pc.queue.Done(key)
			err := pc.syncHandler(key.(string))
			if err != nil {
				glog.Errorf("Error syncing Push: %v", err)
			}
		}()
	}
}

func (pc *PushController) syncPush(key string) error {
	obj, exists, err := pc.pushStore.GetByKey(key)
	if !exists {
		glog.Infof("Push has been deleted %v", key)
		return nil
	}
	if err != nil {
		glog.Infof("Unable to retrieve Push %v from store: %v", key, err)
		pc.queue.Add(key)
		return err
	}
	item := *obj.(*runtime.Unstructured)
	data, err := item.MarshalJSON()
	if err != nil {
		pc.queue.Add(key)
		return err
	}
	push := &pushapi.Push{}
	if err := json.Unmarshal(data, push); err != nil {
		pc.queue.Add(key)
		return err
	}

	if push.Status.Phase == pushapi.PushSucceeded || push.Status.Phase == pushapi.PushFailed {
		return nil
	}

	if push.Labels[defaultLabelKey] != pc.watchNode {
		return nil
	}

	pushErr := pc.commitAndPush(push)
	status := &pushapi.PushStatus{}
	if pushErr != nil {
		status.Phase = pushapi.PushFailed
		status.Message = pushErr.Error()
	} else {
		status.Phase = pushapi.PushSucceeded
		status.Message = "Push succeeded"
	}

	if err := pc.UpdatePushStatus(push, status); err != nil {
		glog.Errorf("failed to update push status: %v", err)
		pc.queue.Add(key)
		return err
	}

	return nil
}

func (pc *PushController) commitAndPush(push *pushapi.Push) error {
	containers, err := pc.dockerClient.ListContainers(dockertypes.ContainerListOptions{All: true})
	if err != nil {
		return err
	}

	pushSecrets := []v1.Secret{}
	for _, secretRef := range push.Spec.ImagePushSecrets {
		secret, err := pc.clientset.Secrets(push.Namespace).Get(secretRef.Name)
		if err != nil {
			glog.Warningf("Unable to retrieve push secret %s/%s for %s/%s due to %v.  The image push may not succeed.", push.Namespace, secretRef.Name, push.Namespace, push.Spec.PodName, err)
			continue
		}
		pushSecrets = append(pushSecrets, *secret)
	}

	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		dockerName, _, err := dockertools.ParseDockerName(c.Names[0])
		if err != nil {
			continue
		}
		if string(dockerName.PodFullName) != (push.Spec.PodName+"_"+push.Namespace) || dockerName.ContainerName != push.Spec.ContainerName {
			continue
		}

		if _, err := pc.dockerClient.CommitContainer(c.ID, dockertypes.ContainerCommitOptions{
			Reference: push.Spec.Image,
		}); err != nil {
			return err
		}

		var username, password string
		if push.Annotations != nil {
			username = push.Annotations[usernameAnnotationKey]
			password = push.Annotations[passwordAnnotationKey]
		}

		dockerPusher := dockerclient.NewDockerPusher(pc.dockerClient, 0, 0)

		if username != "" && password != "" {
			return dockerPusher.Push(push.Spec.Image, username, password)
		}

		return dockerPusher.PushWithSecret(push.Spec.Image, pushSecrets)
	}

	return fmt.Errorf("Unable find container: %v on node: %v", push.Spec.ContainerName, pc.watchNode)
}

func (pc *PushController) UpdatePushStatus(push *pushapi.Push, status *pushapi.PushStatus) error {
	pushClient := pc.dynamicClient.Resource(&unversioned.APIResource{Name: "pushs", Namespaced: true, Kind: "push"}, push.Namespace)

	push.Status = *status
	data, err := json.Marshal(push)
	if err != nil {
		return err
	}

	obj := &runtime.Unstructured{}
	if err := obj.UnmarshalJSON(data); err != nil {
		return err
	}
	if _, err := pushClient.Update(obj); err != nil {
		return err
	}
	return nil
}
