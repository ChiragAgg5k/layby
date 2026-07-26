package blueprint

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Load reads a blueprint from path, or discovers a mise.toml by walking up
// from the working directory when path is empty.
func Load(path string) (*Blueprint, error) {
	if path == "" {
		discovered, err := discover()
		if err != nil {
			return nil, err
		}
		path = discovered
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading blueprint: %w", err)
	}

	blueprint := &Blueprint{Path: path}
	if err := toml.Unmarshal(contents, blueprint); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	blueprint.applyDefaults()
	if err := blueprint.Validate(); err != nil {
		return nil, fmt.Errorf("invalid blueprint %s: %w", path, err)
	}
	return blueprint, nil
}

func discover() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(directory, "mise.toml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("no mise.toml found in %s or any parent directory", directory)
		}
		directory = parent
	}
}

func (b *Blueprint) applyDefaults() {
	if b.Tools == nil {
		b.Tools = map[string]string{}
	}
	if b.Env == nil {
		b.Env = map[string]string{}
	}
	if b.Sandbox.Provider == "" {
		b.Sandbox.Provider = ProviderLocal
	}
	if b.Sandbox.Size == "" {
		b.Sandbox.Size = defaultSize
	}
	if b.Sandbox.Region == "" {
		b.Sandbox.Region = defaultRegion
	}
	if b.Sandbox.TimeToLive.Duration == 0 {
		b.Sandbox.TimeToLive.Duration = defaultTimeToLive
	}
	if b.Sandbox.IdleTimeout.Duration == 0 {
		b.Sandbox.IdleTimeout.Duration = defaultIdleTimeout
	}
	if b.Sandbox.Repository.Directory == "" {
		b.Sandbox.Repository.Directory = defaultDirectory
	}
	if b.Sandbox.Repository.URL != "" && b.Sandbox.Repository.Reference == "" {
		b.Sandbox.Repository.Reference = defaultReference
	}
}

func (b *Blueprint) Validate() error {
	switch b.Sandbox.Provider {
	case ProviderLocal, ProviderRender, ProviderSSH:
	default:
		return fmt.Errorf("unknown provider %q (want local, render or ssh)", b.Sandbox.Provider)
	}

	if len(b.Tools) == 0 && b.Sandbox.Image == "" {
		return fmt.Errorf("blueprint declares no [tools] and no [sandbox].image")
	}

	for name, version := range b.Tools {
		if version == "" {
			return fmt.Errorf("tool %q has an empty version; pin it explicitly", name)
		}
	}
	return nil
}

// sourceBuilders are mise tool specifications that compile from source rather
// than fetching a prebuilt binary. Each one turns a fast image build into a
// slow one, so `sbx doctor` surfaces them rather than letting the cost hide.
var sourceBuilders = map[string]string{
	"python": "use a precompiled build, e.g. python = \"3.13\" resolves via the python-build backend and compiles; prefer `core:python` or an aqua/ubi backend",
	"ruby":   "ruby-build compiles from source; expect several minutes on every image rebuild",
	"perl":   "perl compiles from source",
	"erlang": "erlang compiles from source and is among the slowest builds",
}

// Warning is a non-fatal blueprint issue reported by `sbx doctor`.
type Warning struct {
	Subject string
	Message string
}

func (b *Blueprint) Warnings() []Warning {
	warnings := []Warning{}
	for name := range b.Tools {
		if advice, found := sourceBuilders[name]; found {
			warnings = append(warnings, Warning{
				Subject: "tools." + name,
				Message: "compiles from source at image build time — " + advice,
			})
		}
	}
	if b.Sandbox.TimeToLive.Duration > 4*time.Hour {
		warnings = append(warnings, Warning{
			Subject: "sandbox.ttl",
			Message: "longer than 4h; on metered providers this is the main way credit leaks",
		})
	}
	return warnings
}
