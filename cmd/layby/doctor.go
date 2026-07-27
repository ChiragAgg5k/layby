package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/sandbox"
)

func newDoctorCommand() *cobra.Command {
	var path string

	command := &cobra.Command{
		Use:   "doctor",
		Short: "Reconcile provider state against local state and report orphans",
		Long: "Reconcile what the provider actually has against what the local state\n" +
			"file believes. The provider wins: anything running that we have no\n" +
			"record of is an orphan, and orphans are how credit leaks.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDoctor(command.Context(), path)
		},
	}

	blueprintFlag(command, &path)
	return command
}

func runDoctor(ctx context.Context, path string) error {
	problems := 0

	if print, err := blueprint.Load(path); err == nil {
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
			fmt.Printf("  EXPIRED %s outlived its TTL — `layby down --expired`\n", record.Handle.Identifier)
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
