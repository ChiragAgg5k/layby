// Package local implements the Provider interface on top of the local Docker
// daemon. It exists so the full sandbox lifecycle can be exercised end to end
// with no cloud account and no spend, and it is the reference the metered
// drivers are checked against.
package local

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
	labelIdentifier = "sbx.identifier"
	labelExpires    = "sbx.expires"
	labelImage      = "sbx.image"
	labelSize       = "sbx.size"
	containerPrefix = "sbx-"
	driverName      = "local"
)

type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return driverName }

func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Snapshot:              false,
		Fork:                  false,
		PersistentDisk:        true,
		WarmPool:              false,
		SubMinuteBoot:         true,
		PerSandboxCredentials: true,
		InteractiveShell:      true,
	}
}

func (d *Driver) Create(ctx context.Context, spec provider.Specification) (provider.Handle, error) {
	createdAt := time.Now().UTC()
	expiresAt := createdAt.Add(spec.TimeToLive)

	arguments := []string{
		"run", "--detach",
		"--name", containerPrefix + spec.Identifier,
		"--label", labelIdentifier + "=" + spec.Identifier,
		"--label", labelExpires + "=" + strconv.FormatInt(expiresAt.Unix(), 10),
		"--label", labelImage + "=" + spec.Image,
		"--label", labelSize + "=" + spec.Size,
	}
	for key, value := range spec.Environment {
		arguments = append(arguments, "--env", key+"="+value)
	}
	for key, value := range spec.Labels {
		arguments = append(arguments, "--label", key+"="+value)
	}
	arguments = append(arguments, spec.Image, "sleep", "infinity")

	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Stderr = &stderr
	reference, err := command.Output()
	if err != nil {
		return provider.Handle{}, fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return provider.Handle{
		Identifier: spec.Identifier,
		Provider:   driverName,
		Reference:  strings.TrimSpace(string(reference)),
		Image:      spec.Image,
		Size:       spec.Size,
		Region:     spec.Region,
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}, nil
}

func (d *Driver) Status(ctx context.Context, handle provider.Handle) (provider.State, error) {
	command := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Status}}", containerPrefix+handle.Identifier)
	output, err := command.Output()
	if err != nil {
		return provider.StateNotFound, provider.ErrNotFound
	}

	switch strings.TrimSpace(string(output)) {
	case "running":
		return provider.StateReady, nil
	case "created", "restarting":
		return provider.StatePending, nil
	case "exited", "paused", "dead":
		return provider.StateStopped, nil
	default:
		return provider.StateFailed, nil
	}
}

func (d *Driver) Execute(ctx context.Context, handle provider.Handle, command []string, output io.Writer) (int, error) {
	arguments := append([]string{"exec", containerPrefix + handle.Identifier}, command...)
	execution := exec.CommandContext(ctx, "docker", arguments...)
	execution.Stdout = output
	execution.Stderr = os.Stderr

	if err := execution.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode(), nil
		}
		return -1, fmt.Errorf("docker exec: %w", err)
	}
	return 0, nil
}

// Shell replaces the current process with an interactive shell inside the
// sandbox. It is separate from Execute because it needs a TTY and must not
// capture output.
func (d *Driver) Shell(ctx context.Context, handle provider.Handle) error {
	command := exec.CommandContext(ctx, "docker", "exec", "--interactive", "--tty",
		containerPrefix+handle.Identifier, "/bin/bash", "--login")
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (d *Driver) Destroy(ctx context.Context, handle provider.Handle) error {
	command := exec.CommandContext(ctx, "docker", "rm", "--force", "--volumes", containerPrefix+handle.Identifier)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if strings.Contains(stderr.String(), "No such container") {
			return provider.ErrNotFound
		}
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// List returns every sandbox the daemon knows about, including stopped ones.
// The provider is the source of truth for reconciliation, not the local state
// file, so this must report containers the CLI has no record of.
func (d *Driver) List(ctx context.Context) ([]provider.Handle, error) {
	command := exec.CommandContext(ctx, "docker", "ps", "--all",
		"--filter", "label="+labelIdentifier,
		"--format", "{{json .}}")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	handles := []provider.Handle{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			ID     string `json:"ID"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}

		labels := parseLabels(record.Labels)
		identifier := labels[labelIdentifier]
		if identifier == "" {
			continue
		}

		handle := provider.Handle{
			Identifier: identifier,
			Provider:   driverName,
			Reference:  record.ID,
			Image:      labels[labelImage],
			Size:       labels[labelSize],
		}
		if seconds, err := strconv.ParseInt(labels[labelExpires], 10, 64); err == nil {
			handle.ExpiresAt = time.Unix(seconds, 0).UTC()
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

// parseLabels splits the comma-separated key=value list docker ps emits. Label
// values containing commas cannot round-trip through this format; sbx only
// writes comma-free labels.
func parseLabels(encoded string) map[string]string {
	labels := map[string]string{}
	for _, pair := range strings.Split(encoded, ",") {
		key, value, found := strings.Cut(pair, "=")
		if found {
			labels[key] = value
		}
	}
	return labels
}
