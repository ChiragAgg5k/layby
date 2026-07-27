package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/image"
	"github.com/chiragaggarwal/layby/internal/provider"
	"github.com/chiragaggarwal/layby/internal/sandbox"
)

func newUpCommand() *cobra.Command {
	var (
		path       string
		timeToLive string
		registry   string
		quiet      bool
		rebuild    bool
	)

	command := &cobra.Command{
		Use:   "up",
		Short: "Provision a sandbox and wait until it is ready",
		Long: "Provision a sandbox and wait until it is ready.\n\n" +
			"Only the identifier goes to stdout, so ID=$(layby up) works in a script\n" +
			"and an agent can branch on it without parsing human output.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runUp(command.Context(), path, timeToLive, registry, quiet, rebuild)
		},
	}

	blueprintFlag(command, &path)
	registryFlag(command, &registry)
	command.Flags().StringVar(&timeToLive, "ttl", "", "override the blueprint TTL, e.g. 90m")
	command.Flags().BoolVar(&quiet, "quiet", false, "suppress build output")
	command.Flags().BoolVar(&rebuild, "rebuild", false, "rebuild the image even if it is cached")
	return command
}

func identifier() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func runUp(ctx context.Context, path, timeToLive, registry string, quiet, rebuild bool) error {
	print, err := blueprint.Load(path)
	if err != nil {
		return err
	}
	if timeToLive != "" {
		parsed, err := time.ParseDuration(timeToLive)
		if err != nil {
			return fmt.Errorf("invalid --ttl: %w", err)
		}
		print.Sandbox.TimeToLive.Duration = parsed
	}

	driver, err := driverFor(print.Sandbox.Provider)
	if err != nil {
		return err
	}

	started := time.Now()
	reference, err := image.Reference(registry, print)
	if err != nil {
		return err
	}

	if !rebuild && image.Exists(ctx, reference) {
		fmt.Fprintf(os.Stderr, "image  %s (cached)\n", reference)
	} else {
		fmt.Fprintf(os.Stderr, "image  %s (building — this happens once per toolchain)\n", reference)
		if err := image.Build(ctx, reference, print, !quiet); err != nil {
			return err
		}
	}
	built := time.Now()

	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}

	id := identifier()
	handle, err := driver.Create(ctx, provider.Specification{
		Identifier:  id,
		Image:       reference,
		Size:        print.Sandbox.Size,
		Region:      print.Sandbox.Region,
		Environment: print.Env,
		TimeToLive:  print.Sandbox.TimeToLive.Duration,
		SSHKeys:     print.Sandbox.SSHKeys,
		Labels:      map[string]string{"layby.toolhash": print.ToolHash()},
	})
	if err != nil {
		return err
	}

	if err := store.Add(sandbox.Record{
		Handle:    handle,
		Blueprint: print.Path,
		ToolHash:  print.ToolHash(),
	}); err != nil {
		return err
	}

	if err := waitForReady(ctx, driver, handle); err != nil {
		return err
	}

	for _, hook := range print.Sandbox.Hooks.Setup {
		fmt.Fprintf(os.Stderr, "setup  %s\n", hook)
		code, err := driver.Execute(ctx, handle, []string{"bash", "-c", hook}, os.Stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("setup hook %q exited %d", hook, code)
		}
	}

	fmt.Fprintf(os.Stderr, "ready  %s in %s (build %s, boot %s)\n",
		handle.Identifier,
		time.Since(started).Round(time.Millisecond),
		built.Sub(started).Round(time.Millisecond),
		time.Since(built).Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "expires %s\n", handle.ExpiresAt.Local().Format(time.Kitchen))

	// The identifier alone goes to stdout so `ID=$(layby up)` works in a script
	// and an agent can branch on it without parsing human output.
	fmt.Println(handle.Identifier)
	return nil
}

// waitForReady polls until the provider reports the sandbox usable. Agents
// need a machine-checkable readiness signal, not just a successful create
// call, or they race the boot.
func waitForReady(ctx context.Context, driver provider.Provider, handle provider.Handle) error {
	capabilities := driver.Capabilities()

	interval := capabilities.ReadinessPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := capabilities.ReadinessTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := driver.Status(ctx, handle)
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			return err
		}
		switch state {
		case provider.StateReady:
			return nil
		case provider.StateFailed, provider.StateStopped:
			return fmt.Errorf("sandbox %s entered state %q during boot", handle.Identifier, state)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("sandbox %s did not become ready within %s", handle.Identifier, timeout)
}
