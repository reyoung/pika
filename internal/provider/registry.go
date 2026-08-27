package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type Registry struct {
	adapters     map[string]Adapter
	mu           sync.RWMutex
	capabilities map[string]Capabilities
}

func (r *Registry) Kinds() []string {
	if r == nil {
		return nil
	}
	kinds := make([]string, 0, len(r.adapters))
	for kind := range r.adapters {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]Adapter, len(adapters)), capabilities: make(map[string]Capabilities, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil || adapter.Kind() == "" {
			return nil, errors.New("provider Adapter and kind are required")
		}
		if _, exists := registry.adapters[adapter.Kind()]; exists {
			return nil, fmt.Errorf("provider Adapter %q is already registered", adapter.Kind())
		}
		registry.adapters[adapter.Kind()] = adapter
	}
	return registry, nil
}

// Probe runs the selected Adapter's conformance check and caches the exact
// result used by later Agent Sessions in this daemon process.
func (r *Registry) Probe(ctx context.Context, kind string, request ProbeRequest) (Capabilities, error) {
	adapter, err := r.Resolve(kind)
	if err != nil {
		return Capabilities{}, err
	}
	capabilities, err := adapter.Probe(ctx, request)
	if err != nil {
		return Capabilities{}, err
	}
	if capabilities.Kind != kind {
		return Capabilities{}, fmt.Errorf("provider Probe kind changed: requested=%s returned=%s", kind, capabilities.Kind)
	}
	r.mu.Lock()
	r.capabilities[kind] = capabilities
	r.mu.Unlock()
	return capabilities, nil
}

func (r *Registry) Capabilities(kind string) (Capabilities, bool) {
	if r == nil {
		return Capabilities{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	capabilities, found := r.capabilities[kind]
	return capabilities, found
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(NewCodexAdapter(), NewCursorAdapter())
	if err != nil {
		panic(err)
	}
	return registry
}

func (r *Registry) Resolve(kind string) (Adapter, error) {
	if r == nil {
		return nil, errors.New("provider registry is required")
	}
	adapter, found := r.adapters[kind]
	if !found {
		return nil, fmt.Errorf("unsupported Agent kind %q", kind)
	}
	return adapter, nil
}
