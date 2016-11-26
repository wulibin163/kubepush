package dockerclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	dockermessage "github.com/docker/docker/pkg/jsonmessage"
	dockerapi "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"
)

const (
	defaultTimeout                            = 2 * time.Minute
	defaultImagePushingProgressReportInterval = 10 * time.Second
	defaultImagePushingStuckTimeout           = 1 * time.Minute
)

type DockerInterface interface {
	dockertools.DockerInterface
	CommitPushInterface
}

type CommitPushInterface interface {
	CommitContainer(container string, opts dockertypes.ContainerCommitOptions) (string, error)
	PushImage(image string, auth dockertypes.AuthConfig, opts dockertypes.ImagePushOptions) error
}

func NewDockerClient(dockerEndpoint string, requestTimeout time.Duration) DockerInterface {
	client := dockertools.ConnectToDockerOrDie(dockerEndpoint, requestTimeout)
	commitPushClient := connectToDockerOrDie(dockerEndpoint, requestTimeout)

	return &kubeDockerClientWrap{
		DockerInterface:  client,
		commitPushClient: commitPushClient,
	}
}

type kubeDockerClientWrap struct {
	*commitPushClient
	dockertools.DockerInterface
}

type commitPushClient struct {
	// timeout is the timeout of short running docker operations.
	timeout time.Duration
	client  *dockerapi.Client
}

func connectToDockerOrDie(dockerEndpoint string, requestTimeout time.Duration) *commitPushClient {
	client, err := getDockerClient(dockerEndpoint)
	if err != nil {
		glog.Fatalf("Couldn't connect to docker: %v", err)
	}
	glog.Infof("Start docker client with request timeout=%v", requestTimeout)
	return newCommitPushClient(client, requestTimeout)
}

// Get a *dockerapi.Client, either using the endpoint passed in, or using
// DOCKER_HOST, DOCKER_TLS_VERIFY, and DOCKER_CERT path per their spec
func getDockerClient(dockerEndpoint string) (*dockerapi.Client, error) {
	if len(dockerEndpoint) > 0 {
		glog.Infof("Connecting to docker on %s", dockerEndpoint)
		return dockerapi.NewClient(dockerEndpoint, "", nil, nil)
	}
	return dockerapi.NewEnvClient()
}

// newCommitPushClient creates an newCommitPushClient from an existing docker client. If requestTimeout is 0,
// defaultTimeout will be applied.
func newCommitPushClient(dockerClient *dockerapi.Client, requestTimeout time.Duration) *commitPushClient {
	if requestTimeout == 0 {
		requestTimeout = defaultTimeout
	}
	return &commitPushClient{
		client:  dockerClient,
		timeout: requestTimeout,
	}
}

func (d *commitPushClient) PushImage(image string, auth dockertypes.AuthConfig, opts dockertypes.ImagePushOptions) error {
	// RegistryAuth is the base64 encoded credentials for the registry
	base64Auth, err := base64EncodeAuth(auth)
	if err != nil {
		return err
	}
	opts.RegistryAuth = base64Auth
	ctx, cancel := d.getCancelableContext()
	defer cancel()
	resp, err := d.client.ImagePush(ctx, image, opts)
	if err != nil {
		return err
	}
	defer resp.Close()
	reporter := newProgressReporter(image, cancel)
	reporter.start()
	defer reporter.stop()
	decoder := json.NewDecoder(resp)
	for {
		var msg dockermessage.JSONMessage
		err := decoder.Decode(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if msg.Error != nil {
			return msg.Error
		}
		reporter.set(&msg)
	}
	return nil
}

func (d *commitPushClient) CommitContainer(container string, opts dockertypes.ContainerCommitOptions) (string, error) {
	ctx, cancel := d.getTimeoutContext()
	defer cancel()
	resp, err := d.client.ContainerCommit(ctx, container, opts)
	if ctxErr := contextError(ctx); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil {
		return "", err
	}
	return resp.ID, err
}

func (d *commitPushClient) getCancelableContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// getTimeoutContext returns a new context with default request timeout
func (d *commitPushClient) getTimeoutContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d.timeout)
}

func base64EncodeAuth(auth dockertypes.AuthConfig) (string, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(auth); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf.Bytes()), nil
}

// progress is a wrapper of dockermessage.JSONMessage with a lock protecting it.
type progress struct {
	sync.RWMutex
	// message stores the latest docker json message.
	message *dockermessage.JSONMessage
	// timestamp of the latest update.
	timestamp time.Time
}

func newProgress() *progress {
	return &progress{timestamp: time.Now()}
}

func (p *progress) set(msg *dockermessage.JSONMessage) {
	p.Lock()
	defer p.Unlock()
	p.message = msg
	p.timestamp = time.Now()
}

func (p *progress) get() (string, time.Time) {
	p.RLock()
	defer p.RUnlock()
	if p.message == nil {
		return "No progress", p.timestamp
	}
	// The following code is based on JSONMessage.Display
	var prefix string
	if p.message.ID != "" {
		prefix = fmt.Sprintf("%s: ", p.message.ID)
	}
	if p.message.Progress == nil {
		return fmt.Sprintf("%s%s", prefix, p.message.Status), p.timestamp
	}
	return fmt.Sprintf("%s%s %s", prefix, p.message.Status, p.message.Progress.String()), p.timestamp
}

// progressReporter keeps the newest image pushing progress and periodically report the newest progress.
type progressReporter struct {
	*progress
	image  string
	cancel context.CancelFunc
	stopCh chan struct{}
}

// newProgressReporter creates a new progressReporter for specific image with specified reporting interval
func newProgressReporter(image string, cancel context.CancelFunc) *progressReporter {
	return &progressReporter{
		progress: newProgress(),
		image:    image,
		cancel:   cancel,
		stopCh:   make(chan struct{}),
	}
}

// start starts the progressReporter
func (p *progressReporter) start() {
	go func() {
		ticker := time.NewTicker(defaultImagePushingProgressReportInterval)
		defer ticker.Stop()
		for {
			// TODO(random-liu): Report as events.
			select {
			case <-ticker.C:
				progress, timestamp := p.progress.get()
				// If there is no progress for defaultImagePushingStuckTimeout, cancel the operation.
				if time.Now().Sub(timestamp) > defaultImagePushingStuckTimeout {
					glog.Errorf("Cancel pushing image %q because of no progress for %v, latest progress: %q", p.image, defaultImagePushingStuckTimeout, progress)
					p.cancel()
					return
				}
				glog.V(2).Infof("Pushing image %q: %q", p.image, progress)
			case <-p.stopCh:
				progress, _ := p.progress.get()
				glog.V(2).Infof("Stop pushing image %q: %q", p.image, progress)
				return
			}
		}
	}()
}

// stop stops the progressReporter
func (p *progressReporter) stop() {
	close(p.stopCh)
}

// contextError checks the context, and returns error if the context is timeout.
func contextError(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return operationTimeout{err: ctx.Err()}
	}
	return ctx.Err()
}

// operationTimeout is the error returned when the docker operations are timeout.
type operationTimeout struct {
	err error
}

func (e operationTimeout) Error() string {
	return fmt.Sprintf("operation timeout: %v", e.err)
}

// containerNotFoundError is the error returned by InspectContainer when container not found. We
// add this error type for testability. We don't use the original error returned by engine-api
// because dockertypes.containerNotFoundError is private, we can't create and inject it in our test.
type containerNotFoundError struct {
	ID string
}

func (e containerNotFoundError) Error() string {
	return fmt.Sprintf("no such container: %q", e.ID)
}

// imageNotFoundError is the error returned by InspectImage when image not found.
type imageNotFoundError struct {
	ID string
}

func (e imageNotFoundError) Error() string {
	return fmt.Sprintf("no such image: %q", e.ID)
}
