// Command sbx provisions declarative, disposable sandbox environments from a
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

	"github.com/chiragaggarwal/sbx/internal/blueprint"
	"github.com/chiragaggarwal/sbx/internal/image"
	"github.com/chiragaggarwal/sbx/internal/provider"
	"github.com/chiragaggarwal/sbx/internal/provider/local"
	"github.com/chiragaggarwal/sbx/internal/sandbox"
)

const usage = `sbx — declarative disposable sandboxes from a mise.toml

Usage:
  sbx up      [-f mise.toml] [-ttl 1h]   provision a sandbox and wait until ready
  sbx ls                                 list sandboxes with age and TTL remaining
  sbx run     <id> -- <command...>       run a command, passthrough exit code
  sbx shell   <id>                       interactive shell inside the sandbox
  sbx down    <id> | -all | -expired     destroy sandboxes
  sbx doctor  [-f mise.toml]             reconcile provider state, report orphans
  sbx explain [-f mise.toml]             show resolved blueprint and generated Dockerfile
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
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: %v\n", err)
		os.Exit(1)
	}
}

// resolve returns the driver for a blueprint. Only the local driver is wired
// up today; render and ssh report a clear error rather than a nil dereference.
func resolve(print *blueprint.Blueprint) (*local.Driver, error) {
	switch print.Sandbox.Provider {
	case blueprint.ProviderLocal:
		return local.New(), nil
	default:
		return nil, fmt.Errorf("provider %q is not implemented yet; only %q is wired up",
			print.Sandbox.Provider, blueprint.ProviderLocal)
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

	driver, err := resolve(print)
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
		Labels:      map[string]string{"sbx.toolhash": print.ToolHash()},
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

	// The identifier alone goes to stdout so `ID=$(sbx up)` works in a script
	// and an agent can branch on it without parsing human output.
	fmt.Println(handle.Identifier)
	return nil
}

// waitForReady polls until the provider reports the sandbox usable. Agents
// need a machine-checkable readiness signal, not just a successful create
// call, or they race the boot.
func waitForReady(ctx context.Context, driver *local.Driver, handle provider.Handle) error {
	deadline := time.Now().Add(2 * time.Minute)
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
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("sandbox %s did not become ready within 2m", handle.Identifier)
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

	driver := local.New()
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tPROVIDER\tSTATE\tAGE\tTTL LEFT\tBLUEPRINT")

	now := time.Now()
	for _, record := range records {
		state := provider.State("unknown")
		if record.Handle.Provider == blueprint.ProviderLocal {
			if observed, err := driver.Status(ctx, record.Handle); err == nil {
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
		return errors.New("usage: sbx run <id> -- <command...>")
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

	driver := local.New()
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
		return errors.New("usage: sbx shell <id>")
	}
	store, err := sandbox.OpenStore()
	if err != nil {
		return err
	}
	record, err := store.Find(arguments[0])
	if err != nil {
		return err
	}
	return local.New().Shell(ctx, record.Handle)
}

func commandDown(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("down", flag.ExitOnError)
	all := flags.Bool("all", false, "destroy every sandbox")
	expired := flags.Bool("expired", false, "destroy only sandboxes past their TTL")
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
		if flags.NArg() != 1 {
			return errors.New("usage: sbx down <id> | -all | -expired")
		}
		record, err := findAnywhere(ctx, store, flags.Arg(0))
		if err != nil {
			return err
		}
		targets = []sandbox.Record{record}
	}

	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to destroy")
		return nil
	}

	driver := local.New()
	for _, record := range targets {
		err := driver.Destroy(ctx, record.Handle)
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

	handles, err := local.New().List(ctx)
	if err != nil {
		return sandbox.Record{}, err
	}
	for _, handle := range handles {
		if handle.Identifier == identifier {
			return sandbox.Record{Handle: handle, Blueprint: "(orphan)"}, nil
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

	driver := local.New()
	actual, err := driver.List(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("\nprovider local: %d running, %d tracked locally\n", len(actual), len(records))

	for _, handle := range actual {
		if !known[handle.Identifier] {
			fmt.Printf("  ORPHAN  %s is running but absent from local state — `sbx down %s`\n",
				handle.Identifier, handle.Identifier)
			problems++
		}
	}

	seen := map[string]bool{}
	for _, handle := range actual {
		seen[handle.Identifier] = true
	}
	now := time.Now()
	for _, record := range records {
		if !seen[record.Handle.Identifier] {
			fmt.Printf("  STALE   %s is in local state but gone from the provider\n", record.Handle.Identifier)
			problems++
		}
		if record.Handle.Expired(now) {
			fmt.Printf("  EXPIRED %s outlived its TTL — `sbx down -expired`\n", record.Handle.Identifier)
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
