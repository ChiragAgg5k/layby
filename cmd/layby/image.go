package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chiragaggarwal/layby/internal/blueprint"
	"github.com/chiragaggarwal/layby/internal/image"
)

const imageUsage = `layby image — inspect and materialise a blueprint's build definition

Usage:
  layby image tag     [-f mise.toml] [-registry ghcr.io/you]   print the image reference
  layby image context <dir> [-f mise.toml]                     write the build context

The context subcommand exists so CI can build exactly the definition the CLI
would build locally, without reimplementing the template.
`

func commandImage(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New(imageUsage)
	}

	switch arguments[0] {
	case "tag":
		return imageTag(arguments[1:])
	case "context":
		return imageContext(arguments[1:])
	default:
		return fmt.Errorf("unknown image subcommand %q\n\n%s", arguments[0], imageUsage)
	}
}

func imageTag(arguments []string) error {
	flags := flag.NewFlagSet("image tag", flag.ExitOnError)
	path := flags.String("f", "", "blueprint path")
	registry := flags.String("registry", "", "image registry prefix, e.g. ghcr.io/you")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	print, err := blueprint.Load(*path)
	if err != nil {
		return err
	}
	reference, err := image.Reference(*registry, print)
	if err != nil {
		return err
	}
	fmt.Println(reference)
	return nil
}

func imageContext(arguments []string) error {
	flags := flag.NewFlagSet("image context", flag.ExitOnError)
	path := flags.String("f", "", "blueprint path")
	positional, err := parseInterspersed(flags, arguments)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: layby image context <dir> [-f mise.toml]")
	}
	directory := positional[0]

	print, err := blueprint.Load(*path)
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
