package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/image"
)

func newImageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "image",
		Short: "Inspect or materialise the build definition",
	}
	command.AddCommand(newImageTagCommand(), newImageContextCommand())
	return command
}

func newImageTagCommand() *cobra.Command {
	var path, registry string

	command := &cobra.Command{
		Use:   "tag",
		Short: "Print the image reference for a blueprint",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			print, err := blueprint.Load(path)
			if err != nil {
				return err
			}
			reference, err := image.Reference(registry, print)
			if err != nil {
				return err
			}
			fmt.Println(reference)
			return nil
		},
	}

	blueprintFlag(command, &path)
	registryFlag(command, &registry)
	return command
}

func newImageContextCommand() *cobra.Command {
	var path string

	command := &cobra.Command{
		Use:   "context <dir>",
		Short: "Write the build context so CI builds exactly what the CLI would",
		Long: "Write the build context so CI builds exactly what the CLI would.\n\n" +
			"This exists so the pipeline never reimplements the Dockerfile template.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, arguments []string) error {
			return writeImageContext(path, arguments[0])
		},
	}

	blueprintFlag(command, &path)
	return command
}

func writeImageContext(path, directory string) error {
	print, err := blueprint.Load(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("creating context directory: %w", err)
	}

	dockerfile, err := image.Dockerfile(print)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("writing dockerfile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "mise.toml"), []byte(print.MiseConfiguration()), 0o644); err != nil {
		return fmt.Errorf("writing mise.toml: %w", err)
	}

	fmt.Fprintf(os.Stderr, "wrote build context to %s\n", directory)
	return nil
}
