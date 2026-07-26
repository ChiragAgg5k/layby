package main

import (
	"flag"
	"testing"
)

// Go's flag package stops at the first positional argument, so a flag written
// after one was silently ignored — `image context ./dir -f mise.toml` dropped
// the blueprint path and fell back to discovery.
func TestParseInterspersedAcceptsFlagsAfterPositionals(t *testing.T) {
	cases := []struct {
		name      string
		arguments []string
	}{
		{"flag first", []string{"-f", "mise.toml", "./dir"}},
		{"positional first", []string{"./dir", "-f", "mise.toml"}},
		{"equals form after positional", []string{"./dir", "-f=mise.toml"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			path := flags.String("f", "", "blueprint path")

			positional, err := parseInterspersed(flags, testCase.arguments)
			if err != nil {
				t.Fatalf("parseInterspersed: %v", err)
			}
			if *path != "mise.toml" {
				t.Errorf("path = %q, want mise.toml", *path)
			}
			if len(positional) != 1 || positional[0] != "./dir" {
				t.Errorf("positional = %v, want [./dir]", positional)
			}
		})
	}
}

func TestParseInterspersedCollectsMultiplePositionals(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	all := flags.Bool("all", false, "all")

	positional, err := parseInterspersed(flags, []string{"first", "-all", "second"})
	if err != nil {
		t.Fatalf("parseInterspersed: %v", err)
	}
	if !*all {
		t.Error("-all was not parsed")
	}
	if len(positional) != 2 || positional[0] != "first" || positional[1] != "second" {
		t.Errorf("positional = %v, want [first second]", positional)
	}
}

func TestParseInterspersedWithNoArguments(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	positional, err := parseInterspersed(flags, nil)
	if err != nil {
		t.Fatalf("parseInterspersed: %v", err)
	}
	if len(positional) != 0 {
		t.Errorf("positional = %v, want empty", positional)
	}
}
