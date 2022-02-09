package mutator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/telepresenceio/telepresence/v2/pkg/install/agent"

	"gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/install"
	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
)

const AgentsConfigMap = "telepresence-agents"

type agentInjectorConfig struct {
	Namespaced bool     `json:"namespaced"`
	Namespaces []string `json:"namespaces,omitempty"`
}

type Map interface {
	GetInto(string, string, interface{}) (bool, error)
	Run(ctx context.Context) error
}

func decode(v string, into interface{}) error {
	return yaml.NewDecoder(strings.NewReader(v)).Decode(into)
}

func loadAgentConfigs(ctx context.Context, namespace string) (m Map, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	ac := agentInjectorConfig{}
	cm, err := k8sapi.GetK8sInterface(ctx).CoreV1().ConfigMaps(namespace).Get(ctx, AgentsConfigMap, meta.GetOptions{})
	if err == nil {
		if v, ok := cm.Data["agentInjector"]; ok {
			err = decode(v, &ac)
			if err != nil {
				return nil, err
			}
			dlog.Infof(ctx, "using %q entry from ConfigMap %s", "agentInjector", AgentsConfigMap)
		}
	}

	dlog.Infof(ctx, "Loading ConfigMaps from %v", ac.Namespaces)
	return newConfigWatch(AgentsConfigMap, ac.Namespaces...), nil
}

func triggerRollout(ctx context.Context, e *entry) {
	if e.name == "agentInjector" {
		return
	}

	ac := agent.Config{}
	if err := decode(e.value, &ac); err != nil {
		dlog.Errorf(ctx, "Failed to decode ConfigMap entry %q into an agent config", e.value)
		return
	}

	// TODO: Value will contain workload, so pass it here
	wl, err := k8sapi.GetWorkload(ctx, e.name, e.namespace, ac.WorkloadKind)
	if err != nil {
		dlog.Errorf(ctx, "unable to get %s %s.%s: %v", ac.WorkloadKind, e.name, e.namespace, err)
		return
	}
	restartAnnotation := fmt.Sprintf(
		`{"spec": {"template": {"metadata": {"annotations": {"%srestartedAt": "%s"}}}}}`,
		install.DomainPrefix,
		time.Now().Format(time.RFC3339),
	)
	if err = wl.Patch(ctx, types.StrategicMergePatchType, []byte(restartAnnotation)); err != nil {
		dlog.Errorf(ctx, "unable to patch %s %s.%s: %v", ac.WorkloadKind, e.name, e.namespace, err)
		return
	}
	dlog.Infof(ctx, "Successfully rolled out %s.%s", e.name, e.namespace)
}

func newConfigWatch(name string, namespaces ...string) *configWatcher {
	return &configWatcher{
		name:       name,
		namespaces: namespaces,
	}
}

type configWatcher struct {
	sync.RWMutex
	name       string
	namespaces []string
	data       map[string]map[string]string
	modCh      chan entry
	delCh      chan entry
}

type entry struct {
	name      string
	namespace string
	value     string
}

func (c *configWatcher) Run(ctx context.Context) error {
	addCh, delCh, err := c.Start(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-delCh:
			dlog.Infof(ctx, "del %s.%s: %s", e.name, e.namespace, e.value)
			triggerRollout(ctx, &e)
		case e := <-addCh:
			dlog.Infof(ctx, "add %s.%s: %s", e.name, e.namespace, e.value)
			triggerRollout(ctx, &e)
		}
	}
	return nil
}

func (c *configWatcher) GetInto(key, ns string, into interface{}) (bool, error) {
	c.RLock()
	var v string
	vs, ok := c.data[ns]
	if ok {
		v, ok = vs[key]
	}
	c.RUnlock()
	if !ok {
		return false, nil
	}
	if err := decode(v, into); err != nil {
		return false, err
	}
	return true, nil
}

func (c *configWatcher) Start(ctx context.Context) (modCh <-chan entry, delCh <-chan entry, err error) {
	c.Lock()
	c.modCh = make(chan entry)
	c.delCh = make(chan entry)
	c.data = make(map[string]map[string]string)
	c.Unlock()

	api := k8sapi.GetK8sInterface(ctx).CoreV1()
	do := func(ns string) error {
		w, err := api.ConfigMaps(ns).Watch(ctx, meta.SingleObject(meta.ObjectMeta{
			Name: AgentsConfigMap,
		}))
		if err != nil {
			return fmt.Errorf("unable to create watcher: %w", err)
		}
		go c.eventHandler(ctx, w.ResultChan())
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	if len(c.namespaces) == 0 {
		if err = do(""); err != nil {
			return nil, nil, fmt.Errorf("unable to create watcher: %w", err)
		}
	} else {
		for _, ns := range c.namespaces {
			if err = do(ns); err != nil {
				return nil, nil, fmt.Errorf("unable to create watcher: %w", err)
			}
		}
	}
	return c.modCh, c.delCh, nil
}

func (c *configWatcher) eventHandler(ctx context.Context, evCh <-chan watch.Event) {
	defer dlog.Info(ctx, "Ended")
	dlog.Info(ctx, "Started")
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-evCh:
			switch event.Type {
			case watch.Deleted:
				if m, ok := event.Object.(*core.ConfigMap); ok {
					dlog.Infof(ctx, "%s %s.%s", event.Type, m.Name, m.Namespace)
					c.update(ctx, m.Namespace, nil)
				}
			case watch.Added, watch.Modified:
				if m, ok := event.Object.(*core.ConfigMap); ok {
					dlog.Infof(ctx, "%s %s.%s", event.Type, m.Name, m.Namespace)
					if m.Name != AgentsConfigMap {
						continue
					}
					c.update(ctx, m.Namespace, m.Data)
				}
			}
		}
	}
}

func (c *configWatcher) update(ctx context.Context, ns string, m map[string]string) {
	var dels []entry
	var mods []entry
	c.Lock()
	data, ok := c.data[ns]
	if !ok {
		data = make(map[string]string, len(m))
		c.data[ns] = data
	}
	for k, v := range data {
		if _, ok := m[k]; !ok {
			delete(data, k)
			dels = append(dels, entry{name: k, namespace: ns, value: v})
		}
	}
	for k, v := range m {
		if ov, ok := data[k]; ok && ov == v {
			continue
		}
		mods = append(mods, entry{name: k, namespace: ns, value: v})
		data[k] = v
	}
	c.Unlock()

	writeToChan := func(es []entry, ch chan<- entry) {
		for _, e := range es {
			select {
			case <-ctx.Done():
				return
			case ch <- e:
			}
		}
	}
	go writeToChan(dels, c.delCh)
	go writeToChan(mods, c.modCh)
}
