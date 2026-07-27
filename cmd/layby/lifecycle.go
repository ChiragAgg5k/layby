package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chiragaggarwal/layby/internal/provider"
	"github.com/chiragaggarwal/layby/internal/sandbox"
)

func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sandboxes with age and TTL remaining",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runList(command.Context())
		},
	}
}

func runList(ctx context.Context) error {
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

func newRunCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "run <id> -- <command...>",
		Short: "Run a command in a sandbox, passing its exit code through",
		Long: "Run a command in a sandbox, passing its exit code through.\n\n" +
			"Everything after -- is sent to the sandbox verbatim, so an agent can\n" +
			"run `layby run $ID -- pytest` and branch on $?.",
		Args: func(command *cobra.Command, arguments []string) error {
			dash := command.ArgsLenAtDash()
			if dash != 1 || len(arguments) < 2 {
				return errors.New("usage: layby run <id> -- <command...>")
			}
			return nil
		},
		RunE: func(command *cobra.Command, arguments []string) error {
			return runExec(command.Context(), arguments[0], arguments[1:])
		},
	}
	return command
}

func runExec(ctx context.Context, id string, command []string) error {
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
	shellCommand := []string{"bash", "-c", shellQuote(command)}
	code, err := driver.Execute(ctx, record.Handle, shellCommand, os.Stdout)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError{code: code}
	}
	return nil
}

func newShellCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "shell <id>",
		Aliases: []string{"ssh"},
		Short:   "Open an interactive shell inside a sandbox",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
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
			return driver.Shell(command.Context(), record.Handle)
		},
	}
}

func newDownCommand() *cobra.Command {
	var all, expired bool

	command := &cobra.Command{
		Use:   "down [id]",
		Short: "Destroy sandboxes",
		Long: "Destroy sandboxes.\n\n" +
			"Teardown is scoped to the sandboxes layby created, so --all never\n" +
			"touches the rest of the machines on your account.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return runDown(command.Context(), arguments, all, expired)
		},
	}

	command.Flags().BoolVar(&all, "all", false, "destroy every sandbox layby created")
	command.Flags().BoolVar(&expired, "expired", false, "destroy only sandboxes past their TTL")
	return command
}

func runDown(ctx context.Context, arguments []string, all, expired bool) error {
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
	case all:
		targets = records
	case expired:
		for _, record := range records {
			if record.Handle.Expired(now) {
				targets = append(targets, record)
			}
		}
	default:
		if len(arguments) != 1 {
			return errors.New("usage: layby down <id> | --all | --expired")
		}
		record, err := findAnywhere(ctx, store, arguments[0])
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
