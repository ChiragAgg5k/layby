package main

import (
	"fmt"
	"sort"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/provider"
	"github.com/chiragaggarwal/layby/internal/provider/digitalocean"
	"github.com/chiragaggarwal/layby/internal/provider/local"
	"github.com/chiragaggarwal/layby/internal/provider/render"
)

// drivers is the registry of implemented providers. It lives in the command
// rather than the provider package so that each driver can import the shared
// interface without a cycle.
func drivers() map[string]provider.Provider {
	return map[string]provider.Provider{
		blueprint.ProviderLocal:        local.New(),
		blueprint.ProviderRender:       render.New(),
		blueprint.ProviderDigitalOcean: digitalocean.New(),
	}
}

// driverFor resolves a provider by name, reporting the ones that exist rather
// than failing obscurely on an unimplemented backend.
func driverFor(name string) (provider.Provider, error) {
	if driver, found := drivers()[name]; found {
		return driver, nil
	}
	return nil, fmt.Errorf("provider %q is not implemented; available: local, render, digitalocean", name)
}

// sortedDriverNames gives reconciliation a deterministic order so `layby doctor`
// output is stable and diffable between runs.
func sortedDriverNames() []string {
	names := make([]string, 0, len(drivers()))
	for name := range drivers() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
