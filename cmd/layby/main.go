// Command layby provisions declarative, disposable sandbox environments from a
// mise.toml blueprint.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// exitError carries a sandbox command's exit status back to main so `layby run`
// can pass it through. Returning it rather than calling os.Exit inside the
// command keeps deferred cleanup running.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCommand()

	if len(os.Args) < 2 {
		root.SetOut(os.Stderr)
		_ = root.Help()
		os.Exit(2)
	}

	root.SetArgs(normalizeLegacyFlags(os.Args[1:]))

	if err := root.ExecuteContext(ctx); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		fmt.Fprintf(os.Stderr, "layby: %v\n", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "layby",
		Short: "Declarative disposable sandboxes from a mise.toml",
		Long: "layby — declarative disposable sandboxes from a mise.toml\n\n" +
			"The environment is a file in your repo and the machine is whichever\n" +
			"provider you point at. Sandboxes destroy themselves on a timer.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Progress belongs on stderr so stdout stays parseable; see commandUp.
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
	}

	root.AddCommand(
		newUpCommand(),
		newListCommand(),
		newRunCommand(),
		newShellCommand(),
		newDownCommand(),
		newDoctorCommand(),
		newExplainCommand(),
		newImageCommand(),
	)
	return root
}

// legacyLongFlag matches a single-dash long flag such as -ttl or -all=true.
// It deliberately does not match a single-character shorthand like -f.
var legacyLongFlag = regexp.MustCompile(`^-[a-zA-Z][a-zA-Z0-9][a-zA-Z0-9-]*(=.*)?$`)

// normalizeLegacyFlags rewrites single-dash long flags into the double-dash form
// pflag expects.
//
// The CLI shipped on Go's flag package, where -all and --all are the same flag,
// and both the README and the published transcript document the single-dash
// form. pflag reads -all as the shorthand cluster -a -l -l instead, so without
// this every documented invocation would break on upgrade.
//
// Everything after a bare -- is a command to run inside the sandbox and is
// passed through untouched: `layby run $ID -- rg --files` must reach the
// sandbox exactly as written.
func normalizeLegacyFlags(arguments []string) []string {
	normalized := make([]string, 0, len(arguments))
	for index, argument := range arguments {
		if argument == "--" {
			normalized = append(normalized, arguments[index:]...)
			break
		}
		if legacyLongFlag.MatchString(argument) {
			argument = "-" + argument
		}
		normalized = append(normalized, argument)
	}
	return normalized
}

// blueprintFlag registers the -f/--file flag shared by every command that reads
// a blueprint.
func blueprintFlag(command *cobra.Command, target *string) {
	command.Flags().StringVarP(target, "file", "f", "", "blueprint path (default: nearest mise.toml)")
}

// registryFlag registers the image registry prefix flag.
func registryFlag(command *cobra.Command, target *string) {
	command.Flags().StringVar(target, "registry", "", "image registry prefix, e.g. ghcr.io/you")
}

// shellQuote is used by usage strings that show a command template.
func shellQuote(parts []string) string { return strings.Join(parts, " ") }
