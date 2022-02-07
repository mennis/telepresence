package mutator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/datawire/dlib/dgroup"
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
	GetInto(string, interface{}) (bool, error)
}

func loadAgentConfigs(ctx context.Context, namespace string) (acs map[string]Map, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	cw := NewConfigWatch(AgentsConfigMap, namespace)
	if err := RolloutChanges(ctx, cw); err != nil {
		dlog.Error(ctx, err)
		return nil, err
	}
	ac := agentInjectorConfig{}
	found, err := cw.GetInto("agentInjector", &ac)
	if err != nil {
		return nil, err
	}
	if found {
		dlog.Infof(ctx, "using %q entry from ConfigMap %s", "agentInjector", AgentsConfigMap)
	}

	acs = make(map[string]Map)
	acs[namespace] = cw

	var nss []string
	if ac.Namespaced {
		nss = ac.Namespaces
	} else {
		nsList, err := k8sapi.GetK8sInterface(ctx).CoreV1().Namespaces().List(ctx, meta.ListOptions{})
		if err != nil {
			return nil, err
		}
		its := nsList.Items
		for i := range its {
			n := its[i].Name
			if isNamespaceOfInterest(ctx, n) {
				nss = append(nss, its[i].Name)
			}
		}
	}

	dlog.Infof(ctx, "Loading ConfigMaps from %v", nss)
	for _, ns := range nss {
		if _, ok := acs[ns]; ok {
			continue
		}
		cw = NewConfigWatch(AgentsConfigMap, ns)
		if err := RolloutChanges(ctx, cw); err != nil {
			dlog.Error(ctx, err)
			return nil, err
		}
		dlog.Infof(ctx, "Watching %s ConfigMap in namespace %s", AgentsConfigMap, ns)
		acs[ns] = cw
	}
	return acs, nil
}

func triggerRollout(ctx context.Context, ns string, e *Entry) {
	if e.Key == "agentInjector" {
		return
	}

	ac := agentConfig{}
	if err := yaml.NewDecoder(strings.NewReader(e.Value)).Decode(&ac); err != nil {
		dlog.Errorf(ctx, "Failed to decode ConfigMap entry %q into an agent config")
		return
	}

	// TODO: Value will contain workload, so pass it here
	wl, err := k8sapi.GetWorkload(ctx, e.Key, ns, ac.Kind)
	if err != nil {
		dlog.Errorf(ctx, "unable to get %s %s.%s: %v", ac.Kind, e.Key, ns, err)
		return
	}
	restartAnnotation := fmt.Sprintf(
		`{"spec": {"template": {"metadata": {"annotations": {"%srestartedAt": "%s"}}}}}`,
		install.DomainPrefix,
		time.Now().Format(time.RFC3339),
	)
	if err = wl.Patch(ctx, types.StrategicMergePatchType, []byte(restartAnnotation)); err != nil {
		dlog.Errorf(ctx, "unable to patch %s %s.%s: %v", ac.Kind, e.Key, ns, err)
		return
	}
	dlog.Infof(ctx, "Successfully rolled out %s.%s", e.Key, ns)
}

func NewConfigWatch(name, namespace string) *ConfigWatcher {
	return &ConfigWatcher{
		name:      name,
		namespace: namespace,
	}
}

type ConfigWatcher struct {
	sync.RWMutex
	name      string
	namespace string
	data      map[string]string
	addCh     chan Entry
	delCh     chan Entry
}

type Entry struct {
	Key   string
	Value string
}

func RolloutChanges(ctx context.Context, cw *ConfigWatcher) error {
	ctx = dgroup.WithGoroutineName(ctx, fmt.Sprintf("/watch ConfigMap/%s.%s", cw.name, cw.namespace))
	addCh, delCh, err := cw.Start(ctx)
	if err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-delCh:
				dlog.Infof(ctx, "del %s: %s", e.Key, e.Value)
				triggerRollout(ctx, cw.namespace, &e)
			case e := <-addCh:
				dlog.Infof(ctx, "add %s: %s", e.Key, e.Value)
				triggerRollout(ctx, cw.namespace, &e)
			}
		}
	}()
	return nil
}

func (c *ConfigWatcher) GetInto(key string, into interface{}) (bool, error) {
	c.RLock()
	v, ok := c.data[key]
	c.RUnlock()
	if !ok {
		return false, nil
	}
	if err := yaml.NewDecoder(strings.NewReader(v)).Decode(into); err != nil {
		return false, err
	}
	return true, nil
}

func (c *ConfigWatcher) Start(ctx context.Context) (<-chan Entry, <-chan Entry, error) {
	w, err := k8sapi.GetK8sInterface(ctx).CoreV1().ConfigMaps(c.namespace).Watch(ctx, meta.ListOptions{LabelSelector: "app=traffic-manager"})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create watcher: %w", err)
	}
	c.Lock()
	c.addCh = make(chan Entry)
	c.delCh = make(chan Entry)
	c.data = make(map[string]string)
	c.Unlock()
	go c.eventHandler(ctx, w.ResultChan())
	return c.addCh, c.delCh, nil
}

func (c *ConfigWatcher) eventHandler(ctx context.Context, evCh <-chan watch.Event) {
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
					dlog.Infof(ctx, "%s %s", event.Type, m.Name)
					c.update(ctx, nil)
				}
			case watch.Added, watch.Modified:
				if m, ok := event.Object.(*core.ConfigMap); ok {
					dlog.Infof(ctx, "%s %s", event.Type, m.Name)
					if m.Name != AgentsConfigMap {
						continue
					}
					c.update(ctx, m.Data)
				}
			}
		}
	}
}

func (c *ConfigWatcher) update(ctx context.Context, m map[string]string) {
	var dels []Entry
	var adds []Entry
	c.Lock()
	for k, v := range c.data {
		if _, ok := m[k]; !ok {
			delete(c.data, k)
			dels = append(dels, Entry{k, v})
		}
	}
	for k, v := range m {
		if ov, ok := c.data[k]; ok {
			if ov == v {
				continue
			}
			dels = append(dels, Entry{k, ov})
		}
		adds = append(adds, Entry{k, v})
		c.data[k] = v
	}
	c.Unlock()

	writeToChan := func(es []Entry, ch chan<- Entry) {
		for _, e := range es {
			select {
			case <-ctx.Done():
				return
			case ch <- e:
			}
		}
	}
	go writeToChan(dels, c.delCh)
	go writeToChan(adds, c.addCh)
}
