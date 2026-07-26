package image

import (
	"strings"
	"testing"

	"github.com/chiragaggarwal/sbx/internal/blueprint"
)

func fixture(tools map[string]string, packages []string) *blueprint.Blueprint {
	return &blueprint.Blueprint{
		Path:  "/somewhere/mise.toml",
		Tools: tools,
		Sandbox: blueprint.Sandbox{
			Packages:   packages,
			Repository: blueprint.Repository{Directory: "/workspace"},
		},
	}
}

// A blueprint with no extra apt packages previously rendered `procps \ \`,
// where the escaped space broke line continuation and produced an invalid
// Dockerfile.
func TestDockerfileWithoutExtraPackagesHasNoDanglingContinuation(t *testing.T) {
	rendered, err := Dockerfile(fixture(map[string]string{"node": "22"}, nil))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	if strings.Contains(rendered, `\ \`) {
		t.Errorf("dockerfile contains a dangling line continuation:\n%s", rendered)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasSuffix(line, `\`) && strings.HasSuffix(line, ` \ \`) {
			t.Errorf("malformed continuation on line: %q", line)
		}
	}
}

func TestDockerfileIncludesExtraPackages(t *testing.T) {
	rendered, err := Dockerfile(fixture(map[string]string{"node": "22"}, []string{"ripgrep"}))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	if !strings.Contains(rendered, "ripgrep") {
		t.Errorf("expected ripgrep in apt package list:\n%s", rendered)
	}
}

// The image tag must cover the whole build definition. Keying it on the
// toolchain alone meant a corrected Dockerfile silently reused the stale
// image, so the fix appeared to have no effect.
func TestTagChangesWhenBuildDefinitionChanges(t *testing.T) {
	base := fixture(map[string]string{"node": "22"}, nil)
	withPackage := fixture(map[string]string{"node": "22"}, []string{"ripgrep"})

	first, err := Tag(base)
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	second, err := Tag(withPackage)
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if first == second {
		t.Errorf("tag %q did not change when the build definition changed", first)
	}
}

func TestTagIsStableForIdenticalToolchains(t *testing.T) {
	first, err := Tag(fixture(map[string]string{"node": "22", "jq": "1.7.1"}, nil))
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	second, err := Tag(fixture(map[string]string{"jq": "1.7.1", "node": "22"}, nil))
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if first != second {
		t.Errorf("tag is not order-stable: %q vs %q", first, second)
	}
}

// The generated Dockerfile must not embed the blueprint's absolute path, or
// the same toolchain would hash differently on every machine and a prebuilt
// image could never be shared.
func TestTagIsIndependentOfBlueprintPath(t *testing.T) {
	first := fixture(map[string]string{"node": "22"}, nil)
	second := fixture(map[string]string{"node": "22"}, nil)
	second.Path = "/a/completely/different/path/mise.toml"

	firstTag, err := Tag(first)
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	secondTag, err := Tag(second)
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if firstTag != secondTag {
		t.Errorf("tag depends on blueprint path: %q vs %q", firstTag, secondTag)
	}
}

// Tools must resolve from any working directory, which is why the blueprint is
// installed as mise's global config rather than a directory-scoped one.
func TestDockerfileInstallsMiseConfigGlobally(t *testing.T) {
	rendered, err := Dockerfile(fixture(map[string]string{"node": "22"}, nil))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	if !strings.Contains(rendered, "/root/.config/mise/config.toml") {
		t.Errorf("expected the blueprint to be installed as the global mise config:\n%s", rendered)
	}
}

func TestReferenceUsesExplicitImageWhenSet(t *testing.T) {
	print := fixture(map[string]string{"node": "22"}, nil)
	print.Sandbox.Image = "ghcr.io/someone/custom:v1"

	reference, err := Reference("ghcr.io/ignored", print)
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if reference != "ghcr.io/someone/custom:v1" {
		t.Errorf("explicit image was overridden: %q", reference)
	}
}

func TestReferenceAppliesRegistryPrefix(t *testing.T) {
	reference, err := Reference("ghcr.io/chiragaggarwal/", fixture(map[string]string{"node": "22"}, nil))
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if !strings.HasPrefix(reference, "ghcr.io/chiragaggarwal/"+DefaultRepository+":") {
		t.Errorf("unexpected reference %q", reference)
	}
	if strings.Contains(reference, "//") {
		t.Errorf("trailing registry slash was not trimmed: %q", reference)
	}
}
