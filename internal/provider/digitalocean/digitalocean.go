// Package digitalocean implements the Provider interface on top of Droplets.
//
// Droplets are the closest thing to the sandbox primitive this tool actually
// wants: billing is hourly rather than monthly, boot is well under a minute,
// tags are first-class so reconciliation never depends on a naming convention,
// and SSH keys are attached per droplet rather than per account.
//
// A sandbox is a droplet built from DigitalOcean's Docker marketplace image,
// with cloud-init pulling the prebuilt sandbox image and starting one
// long-lived container. Commands then run through `docker exec` over SSH, so
// the execution environment is byte-identical to the local driver's.
package digitalocean

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
	"text/template"
	"time"

	"github.com/chiragaggarwal/sbx/internal/provider"
)

const (
	driverName = "digitalocean"

	// dropletPrefix and the tags below are the provider-side markers that make
	// reconciliation possible without trusting local state.
	dropletPrefix = "sbx-"
	tagManaged    = "sbx"
	tagPrefixID   = "sbx-id-"
	tagPrefixTTL  = "sbx-expires-"

	// baseImage ships Docker preinstalled, which removes an apt install from
	// every single boot.
	baseImage = "docker-20-04"

	// containerName is the single long-lived container each droplet runs.
	containerName = "sbx"

	defaultRegion = "blr1"

	sshOptions = "-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10"
)

// sizeBySpec maps our normalized size onto droplet slugs. Every slug here must
// have at least imageMinimumDisk of disk: DigitalOcean rejects a droplet whose
// disk is smaller than its image, and the cheapest slug on the account
// (s-1vcpu-512mb-10gb) is exactly that case — it fails with a 422 rather than
// degrading, so it is deliberately not offered.
var sizeBySpec = map[string]string{
	"small":    "s-1vcpu-1gb",
	"standard": "s-2vcpu-2gb",
	"large":    "s-4vcpu-8gb",
}

// imageMinimumDisk is the min_disk_size reported by the Docker marketplace
// image, in gigabytes.
const imageMinimumDisk = 25

type Driver struct {
	binary string
}

func New() *Driver { return &Driver{binary: "doctl"} }

func (d *Driver) Name() string { return driverName }

// Capabilities reports one deliberate gap: there is no true in-sandbox
// self-destruct. Doing it properly would mean writing a DigitalOcean API token
// onto a box that runs untrusted agent code, and a full-scope token is far
// worse than an occasional orphan. cloud-init powers the droplet off at expiry
// as a backstop, but a powered-off droplet still bills for its disk — so
// `sbx down -expired` remains the mechanism that actually stops the meter.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Snapshot:              true,
		Fork:                  false,
		PersistentDisk:        true,
		WarmPool:              false,
		SubMinuteBoot:         true,
		PerSandboxCredentials: true,
		InteractiveShell:      true,
	}
}

// cloudInit starts exactly one container from the prebuilt image and marks
// readiness with a file. An agent must be able to distinguish "droplet exists"
// from "sandbox is usable", and the droplet going active says nothing about
// whether the image has finished pulling.
var cloudInit = template.Must(template.New("cloud-init").Parse(`#cloud-config
write_files:
  - path: /usr/local/bin/sbx-boot
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      docker pull {{ .Image }}
      docker run --detach --name {{ .Container }} --restart unless-stopped \
{{- range .Environment }}
        --env {{ . }} \
{{- end }}
        {{ .Image }} sleep infinity
      touch /run/sbx-ready
  - path: /etc/systemd/system/sbx-expire.service
    content: |
      [Unit]
      Description=Power off this sandbox once its TTL has elapsed
      [Service]
      Type=oneshot
      ExecStart=/usr/sbin/shutdown -h now
runcmd:
  - [ bash, -lc, "/usr/local/bin/sbx-boot 2>&1 | tee /var/log/sbx-boot.log" ]
  # A backstop only: this stops the workload but does not stop billing.
  # Destroying the droplet is what stops the meter.
  - [ bash, -lc, "systemd-run --on-active={{ .ExpirySeconds }}s --unit=sbx-expire /usr/sbin/shutdown -h now" ]
`))

type droplet struct {
	Identifier int      `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Tags       []string `json:"tags"`
	Networks   struct {
		V4 []struct {
			Address string `json:"ip_address"`
			Type    string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

func (d droplet) publicAddress() string {
	for _, network := range d.Networks.V4 {
		if network.Type == "public" {
			return network.Address
		}
	}
	return ""
}

func (d *Driver) run(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, d.binary, arguments...)

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		// doctl reports API failures as JSON on stdout and leaves stderr
		// empty, so an error built from stderr alone says nothing at all.
		detail := strings.TrimSpace(stderr.String())
		if extracted := apiErrorDetail(stdout.Bytes()); extracted != "" {
			detail = extracted
		}
		if detail == "" {
			detail = "no error detail reported"
		}
		return nil, fmt.Errorf("doctl %s: %s", strings.Join(arguments, " "), detail)
	}
	return stdout.Bytes(), nil
}

// apiErrorDetail pulls the human-readable message out of doctl's JSON error
// envelope, which it prints on stdout rather than stderr.
func apiErrorDetail(output []byte) string {
	var envelope struct {
		Errors []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil || len(envelope.Errors) == 0 {
		return ""
	}
	details := make([]string, 0, len(envelope.Errors))
	for _, item := range envelope.Errors {
		if item.Detail != "" {
			details = append(details, item.Detail)
		}
	}
	return strings.Join(details, "; ")
}

func (d *Driver) Create(ctx context.Context, spec provider.Specification) (provider.Handle, error) {
	if spec.Image == "" {
		return provider.Handle{}, errors.New("digitalocean sandboxes require a prebuilt image; build and push one first")
	}
	if len(spec.SSHKeys) == 0 {
		available, _ := d.sshKeyNames(ctx)
		return provider.Handle{}, fmt.Errorf(
			"no ssh keys configured; set [sandbox].ssh_keys in the blueprint. Available on this account: %s",
			strings.Join(available, ", "))
	}

	size, found := sizeBySpec[spec.Size]
	if !found {
		return provider.Handle{}, fmt.Errorf("unknown size %q (want small, standard or large)", spec.Size)
	}
	region := spec.Region
	if region == "" {
		region = defaultRegion
	}

	createdAt := time.Now().UTC()
	expiresAt := createdAt.Add(spec.TimeToLive)

	environment := make([]string, 0, len(spec.Environment))
	for key, value := range spec.Environment {
		environment = append(environment, shellQuote(key+"="+value))
	}

	var userData bytes.Buffer
	err := cloudInit.Execute(&userData, struct {
		Image         string
		Container     string
		Environment   []string
		ExpirySeconds int
	}{
		Image:         spec.Image,
		Container:     containerName,
		Environment:   environment,
		ExpirySeconds: int(spec.TimeToLive.Seconds()),
	})
	if err != nil {
		return provider.Handle{}, fmt.Errorf("rendering cloud-init: %w", err)
	}

	userDataFile, err := os.CreateTemp("", "sbx-cloud-init-*.yaml")
	if err != nil {
		return provider.Handle{}, err
	}
	defer os.Remove(userDataFile.Name())
	if _, err := userDataFile.Write(userData.Bytes()); err != nil {
		return provider.Handle{}, err
	}
	userDataFile.Close()

	output, err := d.run(ctx,
		"compute", "droplet", "create", dropletPrefix+spec.Identifier,
		"--image", baseImage,
		"--size", size,
		"--region", region,
		"--ssh-keys", strings.Join(spec.SSHKeys, ","),
		"--tag-names", strings.Join([]string{
			tagManaged,
			tagPrefixID + spec.Identifier,
			tagPrefixTTL + strconv.FormatInt(expiresAt.Unix(), 10),
		}, ","),
		"--user-data-file", userDataFile.Name(),
		"--wait",
		"--output", "json",
	)
	if err != nil {
		return provider.Handle{}, err
	}

	var created []droplet
	if err := json.Unmarshal(output, &created); err != nil || len(created) == 0 {
		return provider.Handle{}, fmt.Errorf("parsing droplet response: %w", err)
	}

	return provider.Handle{
		Identifier: spec.Identifier,
		Provider:   driverName,
		Reference:  strconv.Itoa(created[0].Identifier),
		Address:    created[0].publicAddress(),
		Image:      spec.Image,
		Size:       spec.Size,
		Region:     region,
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}, nil
}

// Status distinguishes an existing droplet from a usable sandbox. cloud-init
// still has to pull the image after the droplet reports active, so readiness
// is the marker file, not the droplet state.
func (d *Driver) Status(ctx context.Context, handle provider.Handle) (provider.State, error) {
	found, err := d.find(ctx, handle.Identifier)
	if err != nil {
		return provider.StateNotFound, provider.ErrNotFound
	}

	switch found.Status {
	case "new":
		return provider.StatePending, nil
	case "off", "archive":
		return provider.StateStopped, nil
	case "active":
		address := found.publicAddress()
		if address == "" {
			return provider.StatePending, nil
		}
		if err := d.secureShell(ctx, address, []string{"test", "-f", "/run/sbx-ready"}, io.Discard); err != nil {
			return provider.StatePending, nil
		}
		return provider.StateReady, nil
	default:
		return provider.StateFailed, nil
	}
}

func (d *Driver) Execute(ctx context.Context, handle provider.Handle, command []string, output io.Writer) (int, error) {
	address, err := d.address(ctx, handle)
	if err != nil {
		return -1, err
	}

	// Wrapping in `docker exec` keeps the execution environment byte-identical
	// to the local driver: same image, same PATH, same mise shims.
	inner := append([]string{"docker", "exec", containerName}, command...)
	if err := d.secureShell(ctx, address, inner, output); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func (d *Driver) Shell(ctx context.Context, handle provider.Handle) error {
	address, err := d.address(ctx, handle)
	if err != nil {
		return err
	}

	arguments := append(strings.Fields(sshOptions), "-t", "root@"+address,
		"docker exec -it "+containerName+" /bin/bash --login")
	command := exec.CommandContext(ctx, "ssh", arguments...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (d *Driver) Destroy(ctx context.Context, handle provider.Handle) error {
	found, err := d.find(ctx, handle.Identifier)
	if err != nil {
		return provider.ErrNotFound
	}
	_, err = d.run(ctx, "compute", "droplet", "delete", strconv.Itoa(found.Identifier), "--force")
	return err
}

// List returns every droplet carrying the managed tag. Tags are the provider's
// own record, so this reports sandboxes the local state file has never heard
// of — which is the entire point of reconciliation.
func (d *Driver) List(ctx context.Context) ([]provider.Handle, error) {
	output, err := d.run(ctx, "compute", "droplet", "list", "--tag-name", tagManaged, "--output", "json")
	if err != nil {
		return nil, err
	}

	// doctl emits "null" rather than an empty array when nothing matches.
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || trimmed == "null" {
		return []provider.Handle{}, nil
	}

	var droplets []droplet
	if err := json.Unmarshal([]byte(trimmed), &droplets); err != nil {
		return nil, fmt.Errorf("parsing droplet list: %w", err)
	}

	handles := make([]provider.Handle, 0, len(droplets))
	for _, found := range droplets {
		handle := provider.Handle{
			Identifier: identifierFromTags(found.Tags, found.Name),
			Provider:   driverName,
			Reference:  strconv.Itoa(found.Identifier),
			Address:    found.publicAddress(),
		}
		if expires, ok := expiryFromTags(found.Tags); ok {
			handle.ExpiresAt = expires
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func (d *Driver) find(ctx context.Context, identifier string) (droplet, error) {
	output, err := d.run(ctx, "compute", "droplet", "list",
		"--tag-name", tagPrefixID+identifier, "--output", "json")
	if err != nil {
		return droplet{}, err
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || trimmed == "null" {
		return droplet{}, provider.ErrNotFound
	}

	var droplets []droplet
	if err := json.Unmarshal([]byte(trimmed), &droplets); err != nil || len(droplets) == 0 {
		return droplet{}, provider.ErrNotFound
	}
	return droplets[0], nil
}

// address prefers the cached handle so the common path costs no API call, and
// falls back to a lookup when the record predates the address being assigned.
func (d *Driver) address(ctx context.Context, handle provider.Handle) (string, error) {
	if handle.Address != "" {
		return handle.Address, nil
	}
	found, err := d.find(ctx, handle.Identifier)
	if err != nil {
		return "", err
	}
	address := found.publicAddress()
	if address == "" {
		return "", fmt.Errorf("sandbox %s has no public address yet", handle.Identifier)
	}
	return address, nil
}

func (d *Driver) secureShell(ctx context.Context, address string, command []string, output io.Writer) error {
	arguments := append(strings.Fields(sshOptions), "root@"+address)
	arguments = append(arguments, command...)

	execution := exec.CommandContext(ctx, "ssh", arguments...)
	execution.Stdout = output
	execution.Stderr = os.Stderr
	return execution.Run()
}

func (d *Driver) sshKeyNames(ctx context.Context) ([]string, error) {
	output, err := d.run(ctx, "compute", "ssh-key", "list", "--output", "json")
	if err != nil {
		return nil, err
	}
	var keys []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &keys); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		names = append(names, key.Name)
	}
	return names, nil
}

func identifierFromTags(tags []string, name string) string {
	for _, tag := range tags {
		if after, found := strings.CutPrefix(tag, tagPrefixID); found {
			return after
		}
	}
	return strings.TrimPrefix(name, dropletPrefix)
}

func expiryFromTags(tags []string) (time.Time, bool) {
	for _, tag := range tags {
		if after, found := strings.CutPrefix(tag, tagPrefixTTL); found {
			if seconds, err := strconv.ParseInt(after, 10, 64); err == nil {
				return time.Unix(seconds, 0).UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// shellQuote wraps a value for safe inclusion in the cloud-init shell script.
// Environment values are user-supplied and reach a root shell on boot, so an
// unquoted value would be a command injection into the sandbox's own bootstrap.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
