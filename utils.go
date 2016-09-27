package main

import (
	"fmt"
	"os"

	"k8s.io/client-go/1.4/kubernetes"
)

func getNodeName(client *kubernetes.Clientset) (string, error) {
	podName := os.Getenv("POD_NAME")
	podNs := os.Getenv("POD_NAMESPACE")

	if podName == "" && podNs == "" {
		return "", fmt.Errorf("unable to get POD information (missing POD_NAME or POD_NAMESPACE environment variable)")
	}

	pod, err := client.Pods(podNs).Get(podName)
	if err != nil {
		return "", err
	}
	return pod.Spec.NodeName, nil
}
