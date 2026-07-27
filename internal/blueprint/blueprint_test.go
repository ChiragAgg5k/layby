package blueprint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mise.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	print, err := Load(write(t, "[tools]\nnode = \"22\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if print.Sandbox.Provider != ProviderLocal {
		t.Errorf("provider = %q, want %q", print.Sandbox.Provider, ProviderLocal)
	}
	if print.Sandbox.TimeToLive.Duration != time.Hour {
		t.Errorf("ttl = %s, want 1h", print.Sandbox.TimeToLive.Duration)
	}
	if print.Sandbox.Repository.Directory != "/workspace" {
		t.Errorf("dir = %q, want /workspace", print.Sandbox.Repository.Directory)
	}
}

func TestLoadParsesSandboxTable(t *testing.T) {
	print, err := Load(write(t, `
[tools]
node = "22"

[sandbox]
provider = "digitalocean"
ttl = "90m"
packages = ["ripgrep"]

[sandbox.repo]
url = "git@github.com:me/thing.git"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if print.Sandbox.Provider != ProviderDigitalOcean {
		t.Errorf("provider = %q", print.Sandbox.Provider)
	}
	if print.Sandbox.TimeToLive.Duration != 90*time.Minute {
		t.Errorf("ttl = %s, want 90m", print.Sandbox.TimeToLive.Duration)
	}
	if print.Sandbox.Repository.Reference != "main" {
		t.Errorf("ref = %q, want the default main", print.Sandbox.Repository.Reference)
	}
	if len(print.Sandbox.Packages) != 1 || print.Sandbox.Packages[0] != "ripgrep" {
		t.Errorf("packages = %v", print.Sandbox.Packages)
	}
}

func TestLoadRejectsUnknownProvider(t *testing.T) {
	if _, err := Load(write(t, "[tools]\nnode = \"22\"\n\n[sandbox]\nprovider = \"heroku\"\n")); err == nil {
		t.Error("expected an error for an unknown provider")
	}
}

func TestLoadRejectsEmptyBlueprint(t *testing.T) {
	if _, err := Load(write(t, "[env]\nFOO = \"bar\"\n")); err == nil {
		t.Error("expected an error when no tools and no image are declared")
	}
}

func TestLoadRejectsNegativeTimeToLive(t *testing.T) {
	if _, err := Load(write(t, "[tools]\nnode = \"22\"\n\n[sandbox]\nttl = \"-5m\"\n")); err == nil {
		t.Error("expected an error for a negative ttl")
	}
}

func TestToolHashIgnoresUnrelatedFields(t *testing.T) {
	first, err := Load(write(t, "[tools]\nnode = \"22\"\n\n[sandbox]\nttl = \"1h\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	second, err := Load(write(t, "[tools]\nnode = \"22\"\n\n[sandbox]\nttl = \"3h\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if first.ToolHash() != second.ToolHash() {
		t.Error("ttl should not affect the toolchain hash")
	}
}

func TestToolHashDistinguishesVersions(t *testing.T) {
	first, err := Load(write(t, "[tools]\nnode = \"22\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	second, err := Load(write(t, "[tools]\nnode = \"20\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if first.ToolHash() == second.ToolHash() {
		t.Error("different tool versions must hash differently")
	}
}

func TestMiseConfigurationIsDeterministic(t *testing.T) {
	print, err := Load(write(t, "[tools]\nnode = \"22\"\njq = \"1.7.1\"\n\n[env]\nB = \"2\"\nA = \"1\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered := print.MiseConfiguration()
	for iteration := 0; iteration < 20; iteration++ {
		if print.MiseConfiguration() != rendered {
			t.Fatal("MiseConfiguration is not deterministic across map iterations")
		}
	}
}

func TestWarningsFlagSourceBuilders(t *testing.T) {
	print, err := Load(write(t, "[tools]\nruby = \"3.3\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(print.Warnings()) == 0 {
		t.Error("expected a warning for a tool that compiles from source")
	}
}

func TestWarningsFlagLongTimeToLive(t *testing.T) {
	print, err := Load(write(t, "[tools]\nnode = \"22\"\n\n[sandbox]\nttl = \"12h\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, warning := range print.Warnings() {
		if warning.Subject == "sandbox.ttl" {
			found = true
		}
	}
	if !found {
		t.Error("expected a warning for a long ttl")
	}
}
