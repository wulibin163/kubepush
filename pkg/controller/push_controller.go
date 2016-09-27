package controller

import (
	"fmt"
	"time"

	"github.com/caicloud/kubepush/pkg/api"
	"github.com/caicloud/kubepush/pkg/dockerclient"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"

	"k8s.io/client-go/1.4/dynamic"
	"k8s.io/client-go/1.4/kubernetes"
	"k8s.io/client-go/1.4/pkg/api/unversioned"
	"k8s.io/client-go/1.4/pkg/api/v1"
	"k8s.io/client-go/1.4/pkg/labels"
	"k8s.io/client-go/1.4/pkg/runtime"
	"k8s.io/client-go/1.4/pkg/util/json"
	"k8s.io/client-go/1.4/pkg/util/wait"
)

const defaultLabelKey = "kubepush.alpha.kubernetes.io/nodename"

type PushController struct {
	dynamicClient *dynamic.Client
	clientset     *kubernetes.Clientset
	dockerClient  dockerclient.DockerInterface

	watchNamespace string
	watchNode      string
}

func NewPushController(clientset *kubernetes.Clientset, dynamicClient *dynamic.Client, dockerClient dockerclient.DockerInterface, namespace, node string) *PushController {
	return &PushController{
		clientset:     clientset,
		dynamicClient: dynamicClient,
		dockerClient:  dockerClient,

		watchNamespace: namespace,
		watchNode:      node,
	}
}

func (pc *PushController) Run(period time.Duration) {
	wait.Forever(func() {
		if err := pc.syncPushs(); err != nil {
			glog.Errorf("faild to sync pushs: %v", err)
		}
	}, period)
}

func (pc *PushController) syncPushs() error {
	options := &v1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{defaultLabelKey: pc.watchNode})}

	objs, err := pc.dynamicClient.Resource(&unversioned.APIResource{Name: "pushs", Namespaced: true, Kind: "push"}, pc.watchNamespace).List(options)
	if err != nil {
		return err
	}

	list := objs.(*runtime.UnstructuredList)
	for _, item := range list.Items {
		data, err := item.MarshalJSON()
		if err != nil {
			glog.Error(err)
			continue
		}

		push := &api.Push{}
		if err := json.Unmarshal(data, push); err != nil {
			glog.Error(err)
			continue
		}

		if push.Status.Phase == api.PushSucceeded || push.Status.Phase == api.PushFailed {
			continue
		}

		if push.Labels[defaultLabelKey] != pc.watchNode {
			glog.Errorf("kakkaka")
			continue
		}

		pc.syncPush(push)
	}

	return nil
}

func (pc *PushController) syncPush(push *api.Push) {
	err := pc.commitAndPush(push)
	status := &api.PushStatus{}
	if err != nil {
		status.Phase = api.PushFailed
		status.Message = err.Error()
	} else {
		status.Phase = api.PushSucceeded
		status.Message = "Push succeeded"
	}
	if err := pc.UpdatePushStatus(push, status); err != nil {
		glog.Errorf("failed to update push status: %v", err)
	}
	return
}

func (pc *PushController) commitAndPush(push *api.Push) error {
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

		dockerPusher := dockerclient.NewDockerPusher(pc.dockerClient, 0, 0)

		return dockerPusher.Push(push.Spec.Image, pushSecrets)
	}

	return fmt.Errorf("Unable find container: %v on node: %v", push.Spec.ContainerName, pc.watchNode)
}

func (pc *PushController) UpdatePushStatus(push *api.Push, status *api.PushStatus) error {
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
