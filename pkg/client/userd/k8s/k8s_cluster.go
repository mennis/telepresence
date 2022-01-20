package k8s

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/blang/semver"
	core "k8s.io/api/core/v1"
	k8err "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
)

const supportedKubeAPIVersion = "1.17.0"

// Cluster is a Kubernetes cluster reference
type Cluster struct {
	*Config
	mappedNamespaces []string

	// Main
	ki kubernetes.Interface

	// nsLock protects currentNamespaces and namespaceListener
	nsLock sync.Mutex

	// Current Namespace snapshot, get set by namespace watcher.
	// The boolean value indicates if this client is allowed to
	// watch services and retrieve workloads in the namespace
	currentNamespaces map[string]bool

	// Current Namespace snapshot, filtered by mappedNamespaces
	currentMappedNamespaces []string

	// Namespace listener. Notified when the currentNamespaces changes
	namespaceListener func(c context.Context)
}

func (kc *Cluster) ActualNamespace(namespace string) string {
	if namespace == "" {
		namespace = kc.Namespace
	}
	if !kc.namespaceExists(namespace) {
		namespace = ""
	}
	return namespace
}

// check uses a non-caching DiscoveryClientConfig to retrieve the server version
func (kc *Cluster) check(c context.Context) error {
	// The discover client is using context.TODO() so the timeout specified in our
	// context has no effect.
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		info, err := k8sapi.GetK8sInterface(c).Discovery().ServerVersion()
		if err != nil {
			errCh <- err
			return
		}
		// Validate that the kubernetes server version is supported
		dlog.Infof(c, "Server version %s", info.GitVersion)
		gitVer, err := semver.Parse(strings.TrimPrefix(info.GitVersion, "v"))
		if err != nil {
			dlog.Errorf(c, "error converting version %s to semver: %s", info.GitVersion, err)
		}
		supGitVer, err := semver.Parse(supportedKubeAPIVersion)
		if err != nil {
			dlog.Errorf(c, "error converting known version %s to semver: %s", supportedKubeAPIVersion, err)
		}
		if gitVer.LT(supGitVer) {
			dlog.Errorf(c,
				"kubernetes server versions older than %s are not supported, using %s .",
				supportedKubeAPIVersion, info.GitVersion)
		}
	}()

	select {
	case <-c.Done():
	case err := <-errCh:
		if err == nil {
			return nil
		}
		if c.Err() == nil {
			return fmt.Errorf("initial cluster check failed: %w", client.RunError(err))
		}
	}
	return c.Err()
}

// FindPodFromSelector returns a pod with the given name-hex-hex
func (kc *Cluster) FindPodFromSelector(c context.Context, namespace string, selector map[string]string) (k8sapi.Object, error) {
	pods, err := k8sapi.Pods(c, namespace)
	if err != nil {
		return nil, err
	}

	for i := range pods {
		podLabels := pods[i].GetLabels()
		match := true
		// check if selector is in labels
		for key, val := range selector {
			if podLabels[key] != val {
				match = false
				break
			}
		}
		if match {
			return pods[i], nil
		}
	}

	return nil, errors.New("pod not found")
}

// FindWorkload returns a workload for the given name, namespace, and workloadKind. The workloadKind
// is optional. A search is performed in the following order if it is empty:
//
//   1. Deployments
//   2. ReplicaSets
//   3. StatefulSets
//
// The first match is returned.
func (kc *Cluster) FindWorkload(c context.Context, namespace, name, workloadKind string) (obj k8sapi.Workload, err error) {
	switch workloadKind {
	case "Deployment":
		obj, err = k8sapi.GetDeployment(c, name, namespace)
	case "ReplicaSet":
		obj, err = k8sapi.GetReplicaSet(c, name, namespace)
	case "StatefulSet":
		obj, err = k8sapi.GetStatefulSet(c, name, namespace)
	case "":
		for _, wk := range []string{"Deployment", "ReplicaSet", "StatefulSet"} {
			if obj, err = kc.FindWorkload(c, namespace, name, wk); err == nil {
				return obj, nil
			}
			if !k8err.IsNotFound(err) {
				return nil, err
			}
		}
		err = k8err.NewNotFound(core.Resource("workload"), name+"."+namespace)
	default:
		return nil, fmt.Errorf("unsupported workload kind: %q", workloadKind)
	}
	return obj, err
}

// FindSvc finds a service with the given name in the given Namespace and returns
// either a copy of that service or nil if no such service could be found.
func (kc *Cluster) FindSvc(c context.Context, namespace, name string) (*core.Service, error) {
	return kc.ki.CoreV1().Services(namespace).Get(c, name, meta.GetOptions{})
}

func (kc *Cluster) namespaceExists(namespace string) (exists bool) {
	kc.nsLock.Lock()
	for _, n := range kc.currentMappedNamespaces {
		if n == namespace {
			exists = true
			break
		}
	}
	kc.nsLock.Unlock()
	return exists
}

func NewCluster(c context.Context, kubeFlags *Config, namespaces []string) (*Cluster, error) {
	rs, err := kubeFlags.ConfigFlags.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(rs)
	if err != nil {
		return nil, err
	}
	c = k8sapi.WithK8sInterface(c, cs)

	if len(namespaces) == 1 && namespaces[0] == "all" {
		namespaces = nil
	} else {
		sort.Strings(namespaces)
	}

	ret := &Cluster{
		Config:            kubeFlags,
		mappedNamespaces:  namespaces,
		ki:                cs,
		currentNamespaces: make(map[string]bool),
	}

	timedC, cancel := client.GetConfig(c).Timeouts.TimeoutContext(c, client.TimeoutClusterConnect)
	defer cancel()
	if err := ret.check(timedC); err != nil {
		return nil, err
	}

	dlog.Infof(c, "Context: %s", ret.Context)
	dlog.Infof(c, "Server: %s", ret.Server)

	ret.startNamespaceWatcher(c)
	return ret, nil
}

func (kc *Cluster) GetCurrentNamespaces() []string {
	kc.nsLock.Lock()
	nss := make([]string, len(kc.currentMappedNamespaces))
	copy(nss, kc.currentMappedNamespaces)
	kc.nsLock.Unlock()
	return nss
}

func (kc *Cluster) GetClusterId(ctx context.Context) string {
	clusterID, _ := k8sapi.GetClusterID(ctx)
	return clusterID
}

func (kc *Cluster) WithK8sInterface(c context.Context) context.Context {
	return k8sapi.WithK8sInterface(c, kc.ki)
}
