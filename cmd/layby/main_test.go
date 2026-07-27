package main

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

// The CLI shipped on Go's flag package, where -all and --all mean the same
// thing, and the README and the published transcript both document the
// single-dash form. pflag reads -all as the shorthand cluster -a -l -l, so
// without normalization every documented invocation breaks on upgrade.
func TestNormalizeLegacyFlagsAcceptsSingleDashLongFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "single dash long flag",
			in:   []string{"down", "-all"},
			want: []string{"down", "--all"},
		},
		{
			name: "value attached with equals",
			in:   []string{"up", "-ttl=90m"},
			want: []string{"up", "--ttl=90m"},
		},
		{
			name: "value as a separate argument",
			in:   []string{"up", "-registry", "ghcr.io/me"},
			want: []string{"up", "--registry", "ghcr.io/me"},
		},
		{
			name: "single character shorthand is left alone",
			in:   []string{"up", "-f", "mise.toml"},
			want: []string{"up", "-f", "mise.toml"},
		},
		{
			name: "already double dashed",
			in:   []string{"down", "--expired"},
			want: []string{"down", "--expired"},
		},
		{
			name: "positional arguments are untouched",
			in:   []string{"down", "b41a0ccd"},
			want: []string{"down", "b41a0ccd"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := normalizeLegacyFlags(testCase.in)
			if !slices.Equal(got, testCase.want) {
				t.Errorf("normalizeLegacyFlags(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

// Everything after a bare -- is a command for the sandbox, not for layby.
// Rewriting a flag there would change what the user asked to run.
func TestNormalizeLegacyFlagsLeavesTheSandboxCommandAlone(t *testing.T) {
	in := []string{"run", "b41a0ccd", "--", "rg", "-uu", "--files", "-l"}
	want := []string{"run", "b41a0ccd", "--", "rg", "-uu", "--files", "-l"}

	got := normalizeLegacyFlags(in)
	if !slices.Equal(got, want) {
		t.Errorf("sandbox command was rewritten:\n got %q\nwant %q", got, want)
	}
}

// The documented teardown invocations have to keep working end to end, not just
// survive string rewriting.
func TestDownParsesBothFlagForms(t *testing.T) {
	for _, arguments := range [][]string{{"-all"}, {"--all"}} {
		command := newDownCommand()
		captured := false
		command.RunE = func(_ *cobra.Command, _ []string) error {
			captured = true
			return nil
		}
		command.SetArgs(normalizeLegacyFlags(arguments))
		command.SetOut(nil)

		if err := command.Execute(); err != nil {
			t.Fatalf("layby down %s: %v", arguments[0], err)
		}
		if !captured {
			t.Fatalf("layby down %s did not reach the command body", arguments[0])
		}
		if all, err := command.Flags().GetBool("all"); err != nil || !all {
			t.Errorf("layby down %s did not set --all (value %v, err %v)", arguments[0], all, err)
		}
	}
}

// `layby run <id> -- <command...>` is the agent-facing contract: the identifier
// is one positional, and everything past the dash is the remote command.
func TestRunSplitsTheIdentifierFromTheRemoteCommand(t *testing.T) {
	command := newRunCommand()

	var gotID string
	var gotCommand []string
	command.RunE = func(cobraCommand *cobra.Command, arguments []string) error {
		gotID = arguments[0]
		gotCommand = arguments[1:]
		return nil
	}
	command.SetArgs([]string{"b41a0ccd", "--", "node", "--version"})

	if err := command.Execute(); err != nil {
		t.Fatalf("layby run: %v", err)
	}
	if gotID != "b41a0ccd" {
		t.Errorf("id = %q, want b41a0ccd", gotID)
	}
	if !slices.Equal(gotCommand, []string{"node", "--version"}) {
		t.Errorf("command = %q, want [node --version]", gotCommand)
	}
}

// Without the dash there is no way to tell an identifier from a command, so the
// CLI should say so rather than guess.
func TestRunRejectsAMissingDash(t *testing.T) {
	command := newRunCommand()
	command.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	command.SetArgs([]string{"b41a0ccd", "node", "--version"})
	command.SilenceUsage = true
	command.SilenceErrors = true

	if err := command.Execute(); err == nil {
		t.Error("expected an error when -- is missing")
	}
}
