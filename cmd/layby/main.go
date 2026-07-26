// Command layby provisions declarative, disposable sandbox environments from a
// mise.toml blueprint.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/image"
	"github.com/chiragaggarwal/layby/internal/provider"
	"github.com/chiragaggarwal/layby/internal/sandbox"
)

const usage = `layby — declarative disposable sandboxes from a mise.toml

Usage:
  layby up      [-f mise.toml] [-ttl 1h]   provision a sandbox and wait until ready
  layby ls                                 list sandboxes with age and TTL remaining
  layby run     <id> -- <command...>       run a command, passthrough exit code
  layby shell   <id>                       interactive shell inside the sandbox
  layby down    <id> | -all | -expired     destroy sandboxes
  layby doctor  [-f mise.toml]             reconcile provider state, report orphans
  layby explain [-f mise.toml]             show resolved blueprint and generated Dockerfile
  layby image   tag|context                inspect or materialise the build definition
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "up":
		err = commandUp(ctx, os.Args[2:])
	case "ls":
		err = commandList(ctx, os.Args[2:])
	case "run":
		err = commandRun(ctx, os.Args[2:])
	case "shell", "ssh":
		err = commandShell(ctx, os.Args[2:])
	case "down":
		err = commandDown(ctx, os.Args[2:])
	case "doctor":
		err = commandDoctor(ctx, os.Args[2:])
	case "explain":
		err = commandExplain(ctx, os.Args[2:])
	case "image":
		err = commandImage(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "layby: %v\n", err)
		os.Exit(1)
	}
}

func identifier() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func commandUp(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("up", flag.ExitOnError)
	path := flags.String("f", "", "blueprint path (default: nearest mise.toml)")
	timeToLive := flags.String("ttl", "", "override the blueprint TTL, e.g. 90m")
	registry := flags.String("registry", "", "image registry prefix, e.g. ghcr.io/you")
	quiet := flags.Bool("quiet", false, "suppress build output")
	rebuild := flags.Bool("rebuild", false, "rebuild the image even if it is cached")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	print, err := blueprint.Load(*path)
	if err != nil {
		return err
	}
	if *timeToLive != "" {
		parsed, err := time.ParseDuration(*timeToLive)
		if err != nil {
			return fmt.Errorf("invalid -ttl: %w", err)
		}
		print.Sandbox.TimeToLive.Duration = parsed
	}

	driver, err := driverFor(print.Sandbox.Provider)
	if err != nil {
		return err
	}

	started := time.Now()
	reference, err := image.Reference(*registry, print)
	if err != nil {
		return err
	}

	if !*rebuild && image.Exists(ctx, reference) {
		fmt.Fprintf(os.Stderr, "image  %s (cached)\n", reference)
	} else {
		fmt.Fprintf(os.Stderr, "image  %s (building — this happens once per toolchain)\n", reference)
		if err := image.Build(ctx, reference, print, !*quiet); err != nil {
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

func commandList(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("ls", flag.ExitOnError)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}
	records, err := store.Load()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "no sandboxes")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tPROVIDER\tSTATE\tAGE\tTTL LEFT\tBLUEPRINT")

	now := time.Now()
	for _, record := range records {
		state := provider.State("unknown")
		if driver, err := driverFor(record.Handle.Provider); err == nil {
			if observed, statusError := driver.Status(ctx, record.Handle); statusError == nil {
				state = observed
			} else {
				state = provider.StateNotFound
			}
		}

		remaining := "—"
		if !record.Handle.ExpiresAt.IsZero() {
			if record.Handle.Expired(now) {
				remaining = "EXPIRED"
			} else {
				remaining = record.Handle.ExpiresAt.Sub(now).Round(time.Second).String()
			}
		}

		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			record.Handle.Identifier,
			record.Handle.Provider,
			state,
			now.Sub(record.Handle.CreatedAt).Round(time.Second),
			remaining,
			record.Blueprint)
	}
	return writer.Flush()
}

func commandRun(ctx context.Context, arguments []string) error {
	separator := -1
	for index, argument := range arguments {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator == -1 || separator == 0 || separator == len(arguments)-1 {
		return errors.New("usage: layby run <id> -- <command...>")
	}

	id := arguments[separator-1]
	command := arguments[separator+1:]

	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}
	record, err := store.Find(id)
	if err != nil {
		return err
	}

	driver, err := driverFor(record.Handle.Provider)
	if err != nil {
		return err
	}
	// Plain -c, not --login: the image's ENV already carries the mise shim
	// path, and sourcing /etc/profile would reset PATH and lose it.
	shellCommand := []string{"bash", "-c", strings.Join(command, " ")}
	code, err := driver.Execute(ctx, record.Handle, shellCommand, os.Stdout)
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

func commandShell(ctx context.Context, arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: layby shell <id>")
	}
	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}
	record, err := store.Find(arguments[0])
	if err != nil {
		return err
	}
	driver, err := driverFor(record.Handle.Provider)
	if err != nil {
		return err
	}
	return driver.Shell(ctx, record.Handle)
}

func commandDown(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("down", flag.ExitOnError)
	all := flags.Bool("all", false, "destroy every sandbox")
	expired := flags.Bool("expired", false, "destroy only sandboxes past their TTL")
	positional, err := parseInterspersed(flags, arguments)
	if err != nil {
		return err
	}

	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}
	records, err := store.Load()
	if err != nil {
		return err
	}

	targets := []sandbox.Record{}
	now := time.Now()
	switch {
	case *all:
		targets = records
	case *expired:
		for _, record := range records {
			if record.Handle.Expired(now) {
				targets = append(targets, record)
			}
		}
	default:
		if len(positional) != 1 {
			return errors.New("usage: layby down <id> | -all | -expired")
		}
		record, err := findAnywhere(ctx, store, positional[0])
		if err != nil {
			return err
		}
		targets = []sandbox.Record{record}
	}

	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to destroy")
		return nil
	}

	for _, record := range targets {
		driver, err := driverFor(record.Handle.Provider)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", record.Handle.Identifier, err)
			continue
		}
		err = driver.Destroy(ctx, record.Handle)
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "warn: destroying %s: %v\n", record.Handle.Identifier, err)
			continue
		}
		if err := store.Remove(record.Handle.Identifier); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "destroyed %s\n", record.Handle.Identifier)
	}
	return nil
}

// findAnywhere resolves an identifier against local state first and the
// provider second. Orphans — running sandboxes with no local record — are
// exactly the case that matters, so refusing to destroy one because the local
// cache has never heard of it would make the leak unfixable by the tool that
// reported it.
func findAnywhere(ctx context.Context, store *sandbox.Store, identifier string) (sandbox.Record, error) {
	if record, err := store.Find(identifier); err == nil {
		return record, nil
	}

	for name, driver := range drivers() {
		handles, err := driver.List(ctx)
		if err != nil {
			// A provider we are not signed in to should not block destroying
			// a sandbox that lives on one we are.
			fmt.Fprintf(os.Stderr, "warn: listing %s: %v\n", name, err)
			continue
		}
		for _, handle := range handles {
			if handle.Identifier == identifier {
				return sandbox.Record{Handle: handle, Blueprint: "(orphan)"}, nil
			}
		}
	}
	return sandbox.Record{}, fmt.Errorf("no sandbox %q in local state or on any provider", identifier)
}

// commandDoctor reconciles what the provider actually has against what the
// local state file believes. The provider wins: anything running that we have
// no record of is an orphan and is the way credit leaks.
func commandDoctor(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	path := flags.String("f", "", "blueprint to lint (default: nearest mise.toml)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	problems := 0

	if print, err := blueprint.Load(*path); err == nil {
		fmt.Printf("blueprint %s\n", print.Path)
		for _, warning := range print.Warnings() {
			fmt.Printf("  warn  %s: %s\n", warning.Subject, warning.Message)
			problems++
		}
	} else {
		fmt.Printf("blueprint  not loaded: %v\n", err)
	}

	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}
	records, err := store.Load()
	if err != nil {
		return err
	}

	known := map[string]bool{}
	for _, record := range records {
		known[record.Handle.Identifier] = true
	}

	// Reconcile every provider, not just the one the current blueprint names.
	// An orphan on a provider you are not using today is exactly the one that
	// keeps billing quietly.
	seen := map[string]bool{}
	for _, name := range sortedDriverNames() {
		actual, err := drivers()[name].List(ctx)
		if err != nil {
			fmt.Printf("\nprovider %s: unavailable (%v)\n", name, err)
			continue
		}

		fmt.Printf("\nprovider %s: %d running\n", name, len(actual))
		for _, handle := range actual {
			seen[handle.Identifier] = true
			if !known[handle.Identifier] {
				fmt.Printf("  ORPHAN  %s is running but absent from local state — `layby down %s`\n",
					handle.Identifier, handle.Identifier)
				problems++
			}
		}
	}
	fmt.Printf("\n%d sandbox(es) tracked locally\n", len(records))
	now := time.Now()
	for _, record := range records {
		if !seen[record.Handle.Identifier] {
			fmt.Printf("  STALE   %s is in local state but gone from the provider\n", record.Handle.Identifier)
			problems++
		}
		if record.Handle.Expired(now) {
			fmt.Printf("  EXPIRED %s outlived its TTL — `layby down -expired`\n", record.Handle.Identifier)
			problems++
		}
	}

	if problems == 0 {
		fmt.Println("\nall clear")
	} else {
		fmt.Printf("\n%d issue(s)\n", problems)
	}
	return nil
}

func commandExplain(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("explain", flag.ExitOnError)
	path := flags.String("f", "", "blueprint path")
	registry := flags.String("registry", "", "image registry prefix")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	print, err := blueprint.Load(*path)
	if err != nil {
		return err
	}

	fmt.Printf("blueprint  %s\n", print.Path)
	fmt.Printf("provider   %s (size %s, region %s)\n", print.Sandbox.Provider, print.Sandbox.Size, print.Sandbox.Region)
	fmt.Printf("ttl        %s (idle %s)\n", print.Sandbox.TimeToLive.Duration, print.Sandbox.IdleTimeout.Duration)
	reference, err := image.Reference(*registry, print)
	if err != nil {
		return err
	}
	fmt.Printf("tool hash  %s\n", print.ToolHash())
	fmt.Printf("image      %s\n", reference)
	fmt.Printf("cached     %t\n", image.Exists(ctx, reference))

	fmt.Printf("\n--- baked mise.toml ---\n%s", print.MiseConfiguration())

	dockerfile, err := image.Dockerfile(print)
	if err != nil {
		return err
	}
	fmt.Printf("\n--- generated Dockerfile ---\n%s", dockerfile)
	return nil
}
