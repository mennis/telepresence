package agent

// Intercept describes the mapping between a service port and an intercepted container port
type Intercept struct {
	// The name of the intercepted container port
	ContainerPortName string `json:"containerPortName,omitempty" yaml:"containerPortName,omitempty"`

	// Name of intercepted service
	ServiceName string `json:"serviceName,omitempty" yaml:"serviceName,omitempty"`

	// L4 protocol used by the intercepted port
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`

	// L7 protocol used by the intercepted port
	AppProtocol string `json:"appProtocol,omitempty" yaml:"appProtocol,omitempty"`

	// The port number that the agent intercepts
	AgentPort int32 `json:"agentPort,omitempty" yaml:"agentPort,omitempty"`

	// The number of the intercepted container port
	ContainerPort int32 `json:"containerPort,omitempty" yaml:"containerPort,omitempty"`
}

// Container describes one container that can have one or several intercepts
type Container struct {
	// Name of the intercepted container
	Name string `json:"name,omitempty" yaml:"name,omitempty"`

	// The intercepts managed by the agent
	Intercepts []*Intercept `json:"intercepts,omitempty" yaml:"intercepts,omitempty"`

	// Prefix used for all keys in the container environment copy
	EnvPrefix string `json:"envPrefix,omitempty" yaml:"envPrefix,omitempty"`

	// Where the agent mounts the agents volumes
	MountPoint string `json:"mountPoint,omitempty" yaml:"mountPoint,omitempty"`
}

// The Config configures the traffic-agent
type Config struct {
	// If Create is true, then this Config has not yet been filled in.
	Create bool `json:"create,omitempty" yaml:"create,omitempty"`

	// The name of the workload that the pod originates from
	WorkloadName string `json:"workloadName,omitempty" yaml:"workloadName,omitempty"`

	// The kind of workload that the pod originates from
	WorkloadKind string `json:"workloadKind,omitempty" yaml:"workloadKind,omitempty"`

	// The host used when connecting to the traffic-manager
	ManagerHost string `json:"managerHost,omitempty" yaml:"managerHost,omitempty"`

	// The port used when connecting to the traffic manager
	ManagerPort int32 `json:"managerPort,omitempty" yaml:"managerPort,omitempty"`

	// The port used by the agents restFUL API server
	APIPort int32 `json:"apiPort,omitempty" yaml:"apiPort,omitempty""`

	// The intercepts managed by the agent
	Containers []*Container `json:"containers,omitempty" yaml:"containers,omitempty"`
}
