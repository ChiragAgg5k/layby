package blueprint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Blueprint is the declarative definition of a sandbox environment. It is
// parsed from a mise.toml file: the [tools] and [env] tables are standard mise
// configuration, and the [sandbox] table is our namespaced extension that plain
// mise ignores. The same file therefore drives both a local `mise install` and
// a remote sandbox.
type Blueprint struct {
	Path    string            `toml:"-"`
	Tools   map[string]string `toml:"tools"`
	Env     map[string]string `toml:"env"`
	Sandbox Sandbox           `toml:"sandbox"`
}

// Sandbox holds the machine and lifecycle configuration for a remote
// environment. Every field has a default so a bare [tools] table is a valid
// blueprint.
type Sandbox struct {
	Provider    string            `toml:"provider"`
	Size        string            `toml:"size"`
	Region      string            `toml:"region"`
	SSHKeys     []string          `toml:"ssh_keys"`
	TimeToLive  Duration          `toml:"ttl"`
	IdleTimeout Duration          `toml:"idle_timeout"`
	Image       string            `toml:"image"`
	Repository  Repository        `toml:"repo"`
	Hooks       Hooks             `toml:"hooks"`
	Packages    []string          `toml:"packages"`
	Secrets     map[string]string `toml:"secrets"`
}

// Repository describes the git source cloned into the sandbox at boot. Git is
// the only ingress and egress path for durable state; the sandbox filesystem
// itself is always ephemeral.
type Repository struct {
	URL       string `toml:"url"`
	Reference string `toml:"ref"`
	Directory string `toml:"dir"`
}

// Hooks are the command lists run at defined points in the sandbox lifecycle.
// Setup runs once after the repository is cloned; Verify is what an agent runs
// to check its own work.
type Hooks struct {
	Setup  []string `toml:"setup"`
	Verify []string `toml:"verify"`
}

const (
	ProviderLocal        = "local"
	ProviderRender       = "render"
	ProviderDigitalOcean = "digitalocean"
	ProviderSSH          = "ssh"

	defaultSize        = "standard"
	defaultTimeToLive  = 1 * time.Hour
	defaultIdleTimeout = 30 * time.Minute
	defaultDirectory   = "/workspace"
	defaultReference   = "main"
)

// Duration wraps time.Duration so TOML can carry human strings like "90m".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	if parsed <= 0 {
		return fmt.Errorf("duration %q must be positive", string(text))
	}
	d.Duration = parsed
	return nil
}

// ToolHash is the cache key for a blueprint's toolchain. It is the content
// hash of the resolved [tools] table plus any extra system packages, and it
// becomes the container image tag. Two blueprints with identical toolchains
// share an image no matter what else differs about them, which is what makes
// the image cache worth having.
func (b *Blueprint) ToolHash() string {
	names := make([]string, 0, len(b.Tools))
	for name := range b.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	digest := sha256.New()
	for _, name := range names {
		fmt.Fprintf(digest, "tool\x00%s\x00%s\x00", name, b.Tools[name])
	}

	packages := append([]string(nil), b.Sandbox.Packages...)
	sort.Strings(packages)
	for _, name := range packages {
		fmt.Fprintf(digest, "package\x00%s\x00", name)
	}

	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// MiseConfiguration renders the [tools] and [env] tables back into a minimal
// mise.toml. This is what gets baked into the image at build time, so the
// expensive `mise install` happens once during an image build rather than on
// every sandbox boot.
func (b *Blueprint) MiseConfiguration() string {
	var builder strings.Builder

	names := make([]string, 0, len(b.Tools))
	for name := range b.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	builder.WriteString("[tools]\n")
	for _, name := range names {
		fmt.Fprintf(&builder, "%q = %q\n", name, b.Tools[name])
	}

	if len(b.Env) > 0 {
		variables := make([]string, 0, len(b.Env))
		for variable := range b.Env {
			variables = append(variables, variable)
		}
		sort.Strings(variables)

		builder.WriteString("\n[env]\n")
		for _, variable := range variables {
			fmt.Fprintf(&builder, "%q = %q\n", variable, b.Env[variable])
		}
	}

	return builder.String()
}
