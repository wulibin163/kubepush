package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caicloud/kubepush/pkg/controller"
	"github.com/caicloud/kubepush/pkg/dockerclient"
	"github.com/golang/glog"
	"github.com/spf13/pflag"

	"k8s.io/client-go/1.4/kubernetes"
	"k8s.io/client-go/1.4/dynamic"
	"k8s.io/client-go/1.4/pkg/api/unversioned"
	"k8s.io/client-go/1.4/pkg/api/v1"
	"k8s.io/client-go/1.4/pkg/util/wait"
	"k8s.io/client-go/1.4/rest"
)

var (
	flags = pflag.NewFlagSet("", pflag.ExitOnError)

	resourceGroup   = flags.String("resource-group", "caicloud.io", `The Group of resource this agent will list and watch`)
	resourceKind    = flags.String("resource-kind", "push", `The Kine of resource this agent will list and watch`)
	resourceVersion = flag.String("resource-version", "v1", `The Version of resource this agent will list and watch`)
	watchNamespace = flags.String("watch-namespace", v1.NamespaceAll,
		`Namespace to watch for Commit. Default is to watch all namespaces`)
	resyncPeriod = flags.Duration("sync-period", 0,
		`Relist and confirm cloud resources this often.`)
	concurrentPushyncs = flags.Int("concurrent-push-syncs", 5,
		`the number of push controllers that are allowed to sync concurrently.
		Larger number = more responsive push management, but more CPU (and network) load"`)
	// dockerEndpoint is the path to the docker endpoint to communicate with.
	dockerEndpoint = flags.String("docker-endpoint", "unix:///var/run/docker.sock",
		`If non-empty, use this for the docker endpoint to communicate with`)
	runtimeRequestTimeout = flags.Duration("runtime-request-timeout", 2*time.Minute,
		`Timeout of all runtime requests except long running request(commit, push).
		 When timeout exceeded, kubelet will cancel the request, throw out an error and retry later.
		 Default: 2m0s")`)
)

func main() {
	flags.AddGoFlagSet(flag.CommandLine)
	flags.Parse(os.Args)

	// workaround of noisy log, see https://github.com/kubernetes/kubernetes/issues/17162
	flag.CommandLine.Parse([]string{})

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	watchNode, err := getNodeName(clientset)
	if err != nil {
		panic(err.Error())
	}

	dynamicClient, err := dynamic.NewClientPool(config, dynamic.LegacyAPIPathResolverFunc).ClientForGroupVersion(unversioned.GroupVersion{Group: *resourceGroup, Version: *resourceVersion})
	if err != nil {
		panic(err.Error())
	}

	dockerClient := dockerclient.NewDockerClient(*dockerEndpoint, *runtimeRequestTimeout)

	pc := controller.NewPushController(clientset, dynamicClient, dockerClient, *watchNamespace, watchNode, *resyncPeriod)

	go handleSigterm(pc)

	pc.Run(*concurrentPushyncs, wait.NeverStop)

}

func handleSigterm(pc *controller.PushController) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	glog.Infof("Received SIGTERM, shutting down")
	os.Exit(0)
}
