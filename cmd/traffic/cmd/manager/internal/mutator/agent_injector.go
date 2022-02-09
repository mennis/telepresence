package mutator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"k8s.io/apimachinery/pkg/labels"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/telepresenceio/telepresence/v2/pkg/install/agent"

	admission "k8s.io/api/admission/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/cmd/traffic/cmd/manager/managerutil"
	"github.com/telepresenceio/telepresence/v2/pkg/install"
	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
)

var podResource = meta.GroupVersionResource{Version: "v1", Group: "", Resource: "pods"}
var findMatchingService = install.FindMatchingService

type agentInjector struct {
	agentConfigs Map
}

func getPod(req *admission.AdmissionRequest) (*core.Pod, error) {
	// Parse the Pod object.
	raw := req.Object.Raw
	pod := core.Pod{}
	if _, _, err := universalDeserializer.Decode(raw, nil, &pod); err != nil {
		return nil, fmt.Errorf("could not deserialize pod object: %v", err)
	}

	podNamespace := pod.Namespace
	if podNamespace == "" {
		// It is very probable the pod was not yet assigned a namespace,
		// in which case we should use the AdmissionRequest namespace.
		pod.Namespace = req.Namespace
	}
	podName := pod.Name
	if podName == "" {
		// It is very probable the pod was not yet assigned a name,
		// in which case we should use the metadata generated name.
		pod.Name = pod.ObjectMeta.GenerateName
	}
	return &pod, nil
}

func (a *agentInjector) inject(ctx context.Context, req *admission.AdmissionRequest) ([]patchOperation, error) {
	// This handler should only get called on Pod objects as per the MutatingWebhookConfiguration in the YAML file.
	// Pod objects are immutable, hence we only care about the CREATE event.
	// Applying patches to Pods instead of Deployments means we don't have side effects on
	// user-managed Deployments and Services. It also means we don't have to manage update flows
	// such as removing or updating the sidecar in Deployments objects... a new Pod just gets created instead!

	// If (for whatever reason) this handler is invoked on an object different from a Pod,
	// issue a log message and let the object request pass through.
	if req.Resource != podResource {
		dlog.Debugf(ctx, "expect resource to be %s, got %s; skipping", podResource, req.Resource)
		return nil, nil
	}

	pod, err := getPod(req)
	if err != nil {
		return nil, err
	}

	// Validate traffic-agent injection preconditions.
	refPodName := fmt.Sprintf("%s.%s", pod.Name, pod.Namespace)
	if pod.Name == "" || pod.Namespace == "" {
		dlog.Debugf(ctx, "Unable to extract pod name and/or namespace (got %q); skipping", refPodName)
		return nil, nil
	}

	hasSidecar := false
	cns := pod.Spec.Containers
	for i := range cns {
		if cns[i].Name == install.AgentContainerName {
			hasSidecar = true
			break
		}
	}

	var config *agent.Config
	switch pod.Annotations[install.InjectAnnotation] {
	case "disabled":
		dlog.Debugf(ctx, `The %s pod is explicitly disabled using a %q annotation; skipping`, refPodName, install.InjectAnnotation)
		return nil, nil
	case "enabled":
		config, err = a.findConfigMapValue(ctx, k8sapi.Pod(pod))
		if err != nil {
			return nil, err
		}
		if config == nil || config.Create {
			config, err = configFromAnnotations(ctx, pod, refPodName)
			if err != nil || !hasSidecar && config == nil {
				return nil, err
			}
		}
	default:
		config, err = a.findConfigMapValue(ctx, k8sapi.Pod(pod))
		if err != nil {
			return nil, err
		}
		if config == nil {
			dlog.Debugf(ctx, `The %s pod has not enabled %s container injection through %q configmap or %q annotation; skipping`,
				refPodName, install.AgentContainerName, AgentsConfigMap, install.InjectAnnotation)
			return nil, nil
		}
		if config.Create {
			config, err = configFromAnnotations(ctx, pod, refPodName)
			if err != nil || !hasSidecar && config == nil {
				return nil, err
			}
		}
	}

	if config != nil {
		sb := strings.Builder{}
		ye := yaml.NewEncoder(&sb)
		ye.SetIndent(2)
		if err := ye.Encode(config); err != nil {
			return nil, err
		}
		dlog.Infof(ctx, sb.String())
	}
	svcName := pod.Annotations[install.ServiceNameAnnotation]
	svc, err := findMatchingService(ctx, "", svcName, pod.Namespace, pod.Labels)
	if err != nil {
		dlog.Error(ctx, err)
		return nil, err
	}

	// The ServicePortAnnotation is expected to contain a string that identifies the service port.
	portNameOrNumber := pod.Annotations[install.ServicePortAnnotation]
	servicePort, appContainer, containerPortIndex, err := install.FindMatchingPort(pod.Spec.Containers, portNameOrNumber, svc)
	if err != nil {
		err := fmt.Errorf("unable to find port to intercept; try the %s annotation: %w", install.ServicePortAnnotation, err)
		dlog.Error(ctx, err)
		return nil, err
	}
	if appContainer.Name == install.AgentContainerName {
		dlog.Infof(ctx, "service %s/%s is already pointing at agent container %s; skipping", svc.Namespace, svc.Name, appContainer.Name)
		return nil, nil
	}

	env := managerutil.GetEnv(ctx)
	ports := appContainer.Ports
	for i := range ports {
		if ports[i].ContainerPort == env.AgentPort {
			err := fmt.Errorf("the %s pod container %s is exposing the same port (%d) as the %s sidecar", refPodName, appContainer.Name, env.AgentPort, install.AgentContainerName)
			dlog.Info(ctx, err)
			return nil, err
		}
	}

	var appPort core.ContainerPort
	switch {
	case containerPortIndex >= 0:
		appPort = appContainer.Ports[containerPortIndex]
	case servicePort.TargetPort.Type == intstr.Int:
		appPort = core.ContainerPort{
			Protocol:      servicePort.Protocol,
			ContainerPort: servicePort.TargetPort.IntVal,
		}
	default:
		// This really shouldn't have happened: the target port is a string, but we weren't able to
		// find a corresponding container port. This should've been caught in FindMatchingPort, but in
		// case it isn't, just return an error.
		return nil, fmt.Errorf("container port unexpectedly not found in %s", refPodName)
	}

	// Create patch operations to add the traffic-agent sidecar
	dlog.Infof(ctx, "Injecting %s into pod %s", install.AgentContainerName, refPodName)

	var patches []patchOperation
	setGID := false
	if servicePort.TargetPort.Type == intstr.Int || svc.Spec.ClusterIP == "None" {
		patches = addInitContainer(ctx, pod, servicePort, &appPort, patches)
		setGID = true
	} else {
		patches = hidePorts(pod, appContainer, servicePort.TargetPort.StrVal, patches)
	}
	tpEnv := make(map[string]string)
	if env.APIPort != 0 {
		tpEnv["TELEPRESENCE_API_PORT"] = strconv.Itoa(int(env.APIPort))
	}
	patches = addTPEnv(pod, appContainer, tpEnv, patches)
	patches, err = addAgentContainer(ctx, svc, pod, servicePort, appContainer, &appPort, setGID, pod.Name, pod.Namespace, patches)
	if err != nil {
		return nil, err
	}
	patches = addAgentVolume(pod, patches)
	return patches, nil
}

func configFromAnnotations(ctx context.Context, pod *core.Pod, refPodName string) (*agent.Config, error) {
	env := managerutil.GetEnv(ctx)
	cns := pod.Spec.Containers
	for i := range cns {
		cn := &cns[i]
		if cn.Name == install.AgentContainerName {
			continue
		}
		ports := cn.Ports
		for pi := range ports {
			if ports[pi].ContainerPort == env.AgentPort {
				return nil, fmt.Errorf(
					"the %s pod container %s is exposing the same port (%d) as the %s sidecar",
					refPodName, cn.Name, env.AgentPort, install.AgentContainerName)
			}
		}
	}

	wl, err := findOwnerWorkload(ctx, k8sapi.Pod(pod))
	if err != nil {
		return nil, err
	}
	if wl == nil {
		dlog.Debugf(ctx, "Unable to find owner workload for pod %s; skipping", refPodName)
		return nil, nil
	}

	svcName := pod.Annotations[install.ServiceNameAnnotation]
	var svcs []k8sapi.Object
	if svcName != "" {
		svc, err := k8sapi.GetService(ctx, svcName, pod.Namespace)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf(
					"unable to find service %s specified by annotation %s declared in pod %s", svcName, install.ServiceNameAnnotation, refPodName)
			}
			return nil, err
		}
		svcs = []k8sapi.Object{svc}
	} else if len(pod.Labels) > 0 {
		lbs := labels.Set(pod.Labels)
		svcs, err = install.FindServicesSelecting(ctx, pod.Namespace, lbs)
		if err != nil {
			return nil, err
		}
		if len(svcs) == 0 {
			return nil, fmt.Errorf("unable to find services that selects pod %s using labels %s", refPodName, lbs)
		}
	} else {
		return nil, fmt.Errorf("unable to resolve a service using pod %s because it has no labels", refPodName)
	}

	portNameOrNumber := pod.Annotations[install.ServicePortAnnotation]
	var ccs []*agent.Container
	ccUnique := make(map[string]*agent.Container, len(cns))
	nic := int32(0)
	for _, svc := range svcs {
		svcImpl, _ := k8sapi.ServiceImpl(svc)
		for _, port := range install.FilterServicePorts(svcImpl, portNameOrNumber) {
			cn, i := install.FindContainerMatchingPort(&port, cns)
			if cn == nil || cn.Name == install.AgentContainerName {
				continue
			}
			var appPort core.ContainerPort
			if i < 0 {
				// Can only happen if the service port is numeric, so it's safe to use TargetPort.IntVal here
				appPort = core.ContainerPort{
					Protocol:      port.Protocol,
					ContainerPort: port.TargetPort.IntVal,
				}
			} else {
				appPort = cn.Ports[i]
			}
			var appProto string
			if port.AppProtocol != nil {
				appProto = *port.AppProtocol
			}

			ic := &agent.Intercept{
				ContainerPortName: appPort.Name,
				ServiceName:       svcImpl.Name,
				Protocol:          string(appPort.Protocol),
				AppProtocol:       appProto,
				AgentPort:         env.AgentPort + nic,
				ContainerPort:     appPort.ContainerPort,
			}
			nic++

			// The container might already have intercepts declared
			cc := ccUnique[cn.Name]
			if cc == nil {
				cc = &agent.Container{
					Name:       cn.Name,
					Intercepts: nil,
					EnvPrefix:  managerutil.CapsBase26(uint64(len(ccs))) + "_",
					MountPoint: install.TelAppMountPoint + "/" + cn.Name,
				}
				ccUnique[cn.Name] = cc
				ccs = append(ccs, cc)
			}
			cc.Intercepts = append(cc.Intercepts, ic)
		}
	}
	if nic == 0 {
		return nil, nil
	}

	return &agent.Config{
		WorkloadName: wl.GetName(),
		WorkloadKind: wl.GetKind(),
		ManagerHost:  install.ManagerAppName + "." + env.ManagerNamespace,
		ManagerPort:  install.ManagerPortHTTP,
		APIPort:      env.APIPort,
		Containers:   ccs,
	}, nil
}

func addInitContainer(ctx context.Context, pod *core.Pod, svcPort *core.ServicePort, appPort *core.ContainerPort, patches []patchOperation) []patchOperation {
	env := managerutil.GetEnv(ctx)
	proto := svcPort.Protocol
	if proto == "" {
		proto = appPort.Protocol
	}
	containerPort := core.ContainerPort{
		Protocol:      proto,
		ContainerPort: env.AgentPort,
	}
	container := install.InitContainer(
		env.AgentRegistry+"/"+env.AgentImage,
		containerPort,
		int(appPort.ContainerPort),
	)

	if pod.Spec.InitContainers == nil {
		patches = append(patches, patchOperation{
			Op:    "add",
			Path:  "/spec/initContainers",
			Value: []core.Container{},
		})
	} else {
		for i, container := range pod.Spec.InitContainers {
			if container.Name == install.InitContainerName {
				if i == len(pod.Spec.InitContainers)-1 {
					return patches
				}
				// If the container isn't the last one, remove it so it can be appended at the end.
				patches = append(patches, patchOperation{
					Op:   "remove",
					Path: fmt.Sprintf("/spec/initContainers/%d", i),
				})
			}
		}
	}

	return append(patches, patchOperation{
		Op:    "add",
		Path:  "/spec/initContainers/-",
		Value: container,
	})
}

func addAgentVolume(pod *core.Pod, patches []patchOperation) []patchOperation {
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == install.AgentAnnotationVolumeName {
			return patches
		}
	}
	return append(patches, patchOperation{
		Op:    "add",
		Path:  "/spec/volumes/-",
		Value: install.AgentVolume(),
	})
}

// addAgentContainer creates a patch operation to add the traffic-agent container
func addAgentContainer(
	ctx context.Context,
	svc *core.Service,
	pod *core.Pod,
	svcPort *core.ServicePort,
	appContainer *core.Container,
	appPort *core.ContainerPort,
	setGID bool,
	podName, namespace string,
	patches []patchOperation,
) ([]patchOperation, error) {
	env := managerutil.GetEnv(ctx)

	refPodName := podName + "." + namespace
	for _, container := range pod.Spec.Containers {
		if container.Name == install.AgentContainerName {
			dlog.Infof(ctx, "Pod %s already has container %s", refPodName, install.AgentContainerName)
			return patches, nil
		}
	}

	dlog.Debugf(ctx, "using service %q port %q when intercepting %s",
		svc.Name,
		func() string {
			if svcPort.Name != "" {
				return svcPort.Name
			}
			return strconv.Itoa(int(svcPort.Port))
		}(), refPodName)

	agentName := ""
	if pod.OwnerReferences != nil {
	owners:
		for _, owner := range pod.OwnerReferences {
			switch owner.Kind {
			case "StatefulSet":
				// If the pod is owned by a statefulset, the workload's name is the same as the statefulset's
				agentName = owner.Name
				break owners
			case "ReplicaSet":
				// If it's owned by a replicaset, then it's the same as the deployment e.g. "my-echo-697464c6c5" -> "my-echo"
				tokens := strings.Split(owner.Name, "-")
				agentName = strings.Join(tokens[:len(tokens)-1], "-")
				break owners
			}
		}
	}
	if agentName == "" {
		// If we weren't able to find a good name for the agent from the owners, take it from the pod name
		agentName = podName
		if strings.HasSuffix(agentName, "-") {
			// Transform a generated name "my-echo-697464c6c5-" into an agent service name "my-echo"
			tokens := strings.Split(podName, "-")
			agentName = strings.Join(tokens[:len(tokens)-2], "-")
		}
	}

	proto := svcPort.Protocol
	if proto == "" {
		proto = appPort.Protocol
	}
	containerPort := core.ContainerPort{
		Protocol:      proto,
		ContainerPort: env.AgentPort,
	}
	if svcPort.TargetPort.Type == intstr.String {
		containerPort.Name = svcPort.TargetPort.StrVal
	}
	patches = append(patches, patchOperation{
		Op:   "add",
		Path: "/spec/containers/-",
		Value: install.AgentContainer(
			agentName,
			env.AgentRegistry+"/"+env.AgentImage,
			appContainer,
			containerPort,
			int(appPort.ContainerPort),
			k8sapi.GetAppProto(ctx, env.AppProtocolStrategy, svcPort),
			int(env.APIPort),
			env.ManagerNamespace,
			setGID,
		)})

	return patches, nil
}

// addTPEnv adds telepresence specific environment variables to the app container
func addTPEnv(pod *core.Pod, cn *core.Container, env map[string]string, patches []patchOperation) []patchOperation {
	if len(env) == 0 {
		return patches
	}
	cns := pod.Spec.Containers
	var containerPath string
	for i := range cns {
		if &cns[i] == cn {
			containerPath = fmt.Sprintf("/spec/containers/%d", i)
			break
		}
	}
	keys := make([]string, len(env))
	i := 0
	for k := range env {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	if cn.Env == nil {
		patches = append(patches, patchOperation{
			Op:    "replace",
			Path:  fmt.Sprintf("%s/%s", containerPath, "env"),
			Value: []core.EnvVar{},
		})
	}
	for _, k := range keys {
		patches = append(patches, patchOperation{
			Op:   "add",
			Path: fmt.Sprintf("%s/%s", containerPath, "env/-"),
			Value: core.EnvVar{
				Name:      k,
				Value:     env[k],
				ValueFrom: nil,
			},
		})
	}
	return patches
}

// hidePorts  will replace the symbolic name of a container port with a generated name. It will perform
// the same replacement on all references to that port from the probes of the container
func hidePorts(pod *core.Pod, cn *core.Container, portName string, patches []patchOperation) []patchOperation {
	cns := pod.Spec.Containers
	var containerPath string
	for i := range cns {
		if &cns[i] == cn {
			containerPath = fmt.Sprintf("/spec/containers/%d", i)
			break
		}
	}

	hiddenPortName := install.HiddenPortName(portName, 0)
	hidePort := func(path string) {
		patches = append(patches, patchOperation{
			Op:    "replace",
			Path:  fmt.Sprintf("%s/%s", containerPath, path),
			Value: hiddenPortName,
		})
	}

	for i, p := range cn.Ports {
		if p.Name == portName {
			hidePort(fmt.Sprintf("ports/%d/name", i))
			break
		}
	}

	probes := []*core.Probe{cn.LivenessProbe, cn.ReadinessProbe, cn.StartupProbe}
	probeNames := []string{"livenessProbe/", "readinessProbe/", "startupProbe/"}

	for i, probe := range probes {
		if probe == nil {
			continue
		}
		if h := probe.HTTPGet; h != nil && h.Port.StrVal == portName {
			hidePort(probeNames[i] + "httpGet/port")
		}
		if t := probe.TCPSocket; t != nil && t.Port.StrVal == portName {
			hidePort(probeNames[i] + "tcpSocket/port")
		}
	}
	return patches
}

func (a *agentInjector) findConfigMapValue(ctx context.Context, obj k8sapi.Object) (*agent.Config, error) {
	if a.agentConfigs == nil {
		return nil, nil
	}
	ag := agent.Config{}
	ok, err := a.agentConfigs.GetInto(obj.GetName(), obj.GetNamespace(), &ag)
	if err != nil {
		return nil, err
	}
	if ok && (ag.WorkloadKind == "" || ag.WorkloadKind == obj.GetKind()) {
		return &ag, nil
	}
	refs := obj.GetOwnerReferences()
	for i := range refs {
		if or := &refs[i]; or.Controller != nil && *or.Controller {
			wl, err := k8sapi.GetWorkload(ctx, or.Name, obj.GetNamespace(), or.Kind)
			if err != nil {
				return nil, err
			}
			return a.findConfigMapValue(ctx, wl)
		}
	}
	return nil, nil
}

func findOwnerWorkload(c context.Context, obj k8sapi.Object) (k8sapi.Object, error) {
	refs := obj.GetOwnerReferences()
	for i := range refs {
		if or := &refs[i]; or.Controller != nil && *or.Controller {
			wl, err := k8sapi.GetWorkload(c, or.Name, obj.GetNamespace(), or.Kind)
			if err != nil {
				return nil, err
			}
			return findOwnerWorkload(c, wl)
		}
	}
	return obj, nil
}
