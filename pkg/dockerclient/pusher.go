package dockerclient

import (
	"fmt"
	"net/http"
	"strings"

	dockerref "github.com/docker/distribution/reference"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/credentialprovider"
	utilerrors "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/flowcontrol"

	"github.com/docker/docker/pkg/jsonmessage"
	internal_api "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/util/parsers"

	"k8s.io/client-go/1.4/pkg/api/v1"
)

// DockerPusher is an abstract interface for testability.  It abstracts image push operations.
type DockerPusher interface {
	PushWithSecret(image string, secrets []v1.Secret) error
	Push(image, username, password string) error
	IsImagePresent(image string) (bool, error)
}

// dockerPusher is the default implementation of DockerPusher.
type dockerPusher struct {
	client  DockerInterface
	keyring credentialprovider.DockerKeyring
}

type throttledDockerPusher struct {
	pusher  dockerPusher
	limiter flowcontrol.RateLimiter
}

// NewDockerPusher creates a new instance of the default implementation of DockerPusher.
func NewDockerPusher(client DockerInterface, qps float32, burst int) DockerPusher {
	dp := dockerPusher{
		client:  client,
		keyring: credentialprovider.NewDockerKeyring(),
	}

	if qps == 0.0 {
		return dp
	}
	return &throttledDockerPusher{
		pusher:  dp,
		limiter: flowcontrol.NewTokenBucketRateLimiter(qps, burst),
	}
}

func (p dockerPusher) Push(image, username, password string) error {
	image, err := applyDefaultImageTag(image)
	if err != nil {
		return err
	}
	pushErr := p.client.PushImage(image, dockertypes.AuthConfig{
		Username: username,
		Password: password,
		Auth:     username,
	}, dockertypes.ImagePushOptions{})
	return filterHTTPError(pushErr, image)
}

func (p dockerPusher) PushWithSecret(image string, secrets []v1.Secret) error {
	// If the image contains no tag or digest, a default tag should be applied.
	image, err := applyDefaultImageTag(image)
	if err != nil {
		return err
	}

	internal_secrets := make([]internal_api.Secret, len(secrets))
	for i := range secrets {
		internal_secrets[i].Type = internal_api.SecretType(secrets[i].Type)
		internal_secrets[i].Data = secrets[i].Data
	}

	keyring, err := credentialprovider.MakeDockerKeyring(internal_secrets, p.keyring)
	if err != nil {
		return err
	}

	opts := dockertypes.ImagePushOptions{}

	creds, haveCredentials := keyring.Lookup(image)
	if !haveCredentials {
		glog.V(1).Infof("Pushing image %s without credentials", image)

		err := p.client.PushImage(image, dockertypes.AuthConfig{}, opts)
		if err == nil {
			return nil
		}

		// Image spec: [<registry>/]<repository>/<image>[:<version] so we count '/'
		explicitRegistry := (strings.Count(image, "/") == 2)
		// Hack, look for a private registry, and decorate the error with the lack of
		// credentials.  This is heuristic, and really probably could be done better
		// by talking to the registry API directly from the kubelet here.
		if explicitRegistry {
			return fmt.Errorf("image push failed for %s, this may be because there are no credentials on this request.  details: (%v)", image, err)
		}

		return filterHTTPError(err, image)
	}

	var pushErrs []error
	for _, currentCreds := range creds {
		err = p.client.PushImage(image, credentialprovider.LazyProvide(currentCreds), opts)
		// If there was no error, return success
		if err == nil {
			return nil
		}

		pushErrs = append(pushErrs, filterHTTPError(err, image))
	}

	return utilerrors.NewAggregate(pushErrs)
}

func (p throttledDockerPusher) PushWithSecret(image string, secrets []v1.Secret) error {
	if p.limiter.TryAccept() {
		return p.pusher.PushWithSecret(image, secrets)
	}
	return fmt.Errorf("push QPS exceeded.")
}

func (p throttledDockerPusher) Push(image, username, password string) error {
	if p.limiter.TryAccept() {
		return p.pusher.Push(image, username, password)
	}
	return fmt.Errorf("push QPS exceeded.")
}

func (p dockerPusher) IsImagePresent(image string) (bool, error) {
	_, err := p.client.InspectImage(image)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(imageNotFoundError); ok {
		return false, nil
	}
	return false, err
}

func (p throttledDockerPusher) IsImagePresent(name string) (bool, error) {
	return p.pusher.IsImagePresent(name)
}

func filterHTTPError(err error, image string) error {
	// docker/docker/push/11314 prints detailed error info for docker push.
	// When it hits 502, it returns a verbose html output including an inline svg,
	// which makes the output of kubectl get pods much harder to parse.
	// Here converts such verbose output to a concise one.
	jerr, ok := err.(*jsonmessage.JSONError)
	if ok && (jerr.Code == http.StatusBadGateway ||
		jerr.Code == http.StatusServiceUnavailable ||
		jerr.Code == http.StatusGatewayTimeout) {
		glog.V(2).Infof("Pushing image %q failed: %v", image, err)
		return images.RegistryUnavailable
	} else {
		return err
	}
}

// applyDefaultImageTag parses a docker image string, if it doesn't contain any tag or digest,
// a default tag will be applied.
func applyDefaultImageTag(image string) (string, error) {
	named, err := dockerref.ParseNamed(image)
	if err != nil {
		return "", fmt.Errorf("couldn't parse image reference %q: %v", image, err)
	}
	_, isTagged := named.(dockerref.Tagged)
	_, isDigested := named.(dockerref.Digested)
	if !isTagged && !isDigested {
		named, err := dockerref.WithTag(named, parsers.DefaultImageTag)
		if err != nil {
			return "", fmt.Errorf("failed to apply default image tag %q: %v", image, err)
		}
		image = named.String()
	}
	return image, nil
}
