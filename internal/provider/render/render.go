// Package render implements the Provider interface on top of Render.
//
// Render is deploy-shaped rather than machine-shaped: there is no create-a-VM
// primitive, so a sandbox is a background worker backed by a prebuilt image.
// That choice is deliberate. Background workers and private services are the
// only Render service types that support both a shell and a persistent disk,
// and pointing one at an already-built image skips Render's build step
// entirely — the toolchain install has already happened in CI.
//
// The driver shells out to the Render CLI rather than calling the REST API so
// it reuses the operator's existing `render login` session instead of asking
// for a separate API key.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/chiragaggarwal/sbx/internal/provider"
)

const (
	driverName    = "render"
	servicePrefix = "sbx-"
	serviceType   = "background_worker"

	// environmentIdentifier is injected so a service can be recognised as ours
	// from the provider side alone. Render has no free-form tagging, and
	// reconciliation must not depend on the local state file.
	environmentIdentifier = "SBX_IDENTIFIER"
	environmentExpires    = "SBX_EXPIRES"
)

// planBySize maps our normalized size onto Render plan names.
var planBySize = map[string]string{
	"small":    "starter",
	"standard": "standard",
	"large":    "pro",
}

type Driver struct {
	binary string
}

func New() *Driver {
	return &Driver{binary: "render"}
}

func (d *Driver) Name() string { return driverName }

// Capabilities is where Render's weaknesses are stated rather than hidden.
// There is no snapshot or fork primitive, boot is deploy-shaped and takes
// minutes, and SSH keys are registered per *account* rather than per service,
// so every sandbox is reachable by every key on the account.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Snapshot:              false,
		Fork:                  false,
		PersistentDisk:        true,
		WarmPool:              false,
		SubMinuteBoot:         false,
		PerSandboxCredentials: false,
		InteractiveShell:      true,
	}
}

// service is the subset of Render's service JSON the driver relies on.
type service struct {
	Identifier  string `json:"id"`
	Name        string `json:"name"`
	Suspended   string `json:"suspended"`
	ServiceType string `json:"type"`
}

func (d *Driver) run(ctx context.Context, arguments ...string) ([]byte, error) {
	arguments = append(arguments, "--confirm", "-o", "json")
	command := exec.CommandContext(ctx, d.binary, arguments...)

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if strings.Contains(message, "402") {
			return nil, fmt.Errorf("render requires a payment method before creating paid services: %s", message)
		}
		return nil, fmt.Errorf("render %s: %w: %s", strings.Join(arguments, " "), err, message)
	}
	return stdout.Bytes(), nil
}

func (d *Driver) Create(ctx context.Context, spec provider.Specification) (provider.Handle, error) {
	if spec.Image == "" {
		return provider.Handle{}, errors.New("render sandboxes require a prebuilt image; build and push one first")
	}

	plan, found := planBySize[spec.Size]
	if !found {
		return provider.Handle{}, fmt.Errorf("unknown size %q (want small, standard or large)", spec.Size)
	}

	createdAt := time.Now().UTC()
	expiresAt := createdAt.Add(spec.TimeToLive)

	arguments := []string{
		"services", "create",
		"--name", servicePrefix + spec.Identifier,
		"--type", serviceType,
		"--image", spec.Image,
		"--plan", plan,
		"--region", spec.Region,
		"--env-var", environmentIdentifier + "=" + spec.Identifier,
		"--env-var", environmentExpires + "=" + strconv.FormatInt(expiresAt.Unix(), 10),
	}
	for key, value := range spec.Environment {
		arguments = append(arguments, "--env-var", key+"="+value)
	}

	output, err := d.run(ctx, arguments...)
	if err != nil {
		return provider.Handle{}, err
	}

	var created service
	if err := json.Unmarshal(output, &created); err != nil {
		return provider.Handle{}, fmt.Errorf("parsing render service response: %w", err)
	}

	return provider.Handle{
		Identifier: spec.Identifier,
		Provider:   driverName,
		Reference:  created.Identifier,
		Image:      spec.Image,
		Size:       spec.Size,
		Region:     spec.Region,
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}, nil
}

// Status reports readiness from the service's latest deploy. Create returning
// successfully only means Render accepted the request; the image still has to
// be pulled and started, so an agent that acts on Create alone races the boot.
func (d *Driver) Status(ctx context.Context, handle provider.Handle) (provider.State, error) {
	output, err := d.run(ctx, "deploys", "list", handle.Reference, "--limit", "1")
	if err != nil {
		return provider.StateNotFound, provider.ErrNotFound
	}

	var deploys []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &deploys); err != nil || len(deploys) == 0 {
		return provider.StatePending, nil
	}

	switch deploys[0].Status {
	case "live":
		return provider.StateReady, nil
	case "created", "queued", "build_in_progress", "update_in_progress", "pre_deploy_in_progress":
		return provider.StatePending, nil
	case "canceled", "deactivated":
		return provider.StateStopped, nil
	default:
		return provider.StateFailed, nil
	}
}

// Execute runs a command over the Render CLI's SSH transport. Once the service
// is live every provider looks the same from here, which is what keeps the
// higher layers free of provider-specific branching.
func (d *Driver) Execute(ctx context.Context, handle provider.Handle, command []string, output io.Writer) (int, error) {
	arguments := append([]string{"ssh", handle.Reference, "--"}, command...)
	execution := exec.CommandContext(ctx, d.binary, arguments...)
	execution.Stdout = output
	execution.Stderr = os.Stderr

	if err := execution.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode(), nil
		}
		return -1, fmt.Errorf("render ssh: %w", err)
	}
	return 0, nil
}

func (d *Driver) Shell(ctx context.Context, handle provider.Handle) error {
	command := exec.CommandContext(ctx, d.binary, "ssh", handle.Reference)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (d *Driver) Destroy(ctx context.Context, handle provider.Handle) error {
	if _, err := d.run(ctx, "services", "delete", handle.Reference); err != nil {
		if strings.Contains(err.Error(), "404") {
			return provider.ErrNotFound
		}
		return err
	}
	return nil
}

// List returns every service whose name carries our prefix. Render has no
// tagging, so the name is the only provider-side marker available — which is
// why Create always prefixes it.
func (d *Driver) List(ctx context.Context) ([]provider.Handle, error) {
	output, err := d.run(ctx, "services")
	if err != nil {
		return nil, err
	}

	var services []service
	if err := json.Unmarshal(output, &services); err != nil {
		return nil, fmt.Errorf("parsing render services: %w", err)
	}

	handles := []provider.Handle{}
	for _, candidate := range services {
		if !strings.HasPrefix(candidate.Name, servicePrefix) {
			continue
		}
		handles = append(handles, provider.Handle{
			Identifier: strings.TrimPrefix(candidate.Name, servicePrefix),
			Provider:   driverName,
			Reference:  candidate.Identifier,
		})
	}
	return handles, nil
}
