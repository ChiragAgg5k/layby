package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/image"
)

func newExplainCommand() *cobra.Command {
	var path, registry string

	command := &cobra.Command{
		Use:   "explain",
		Short: "Show the resolved blueprint and the generated Dockerfile",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runExplain(command.Context(), path, registry)
		},
	}

	blueprintFlag(command, &path)
	registryFlag(command, &registry)
	return command
}

func runExplain(ctx context.Context, path, registry string) error {
	print, err := blueprint.Load(path)
	if err != nil {
		return err
	}

	fmt.Printf("blueprint  %s\n", print.Path)
	fmt.Printf("provider   %s (size %s, region %s)\n", print.Sandbox.Provider, print.Sandbox.Size, print.Sandbox.Region)
	fmt.Printf("ttl        %s (idle %s)\n", print.Sandbox.TimeToLive.Duration, print.Sandbox.IdleTimeout.Duration)
	reference, err := image.Reference(registry, print)
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
