package main

import "flag"

// parseInterspersed parses a flag set that may have positional arguments mixed
// in among the flags, and returns the positionals in order.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `image context ./dir -f mise.toml` would silently drop the -f. People write
// flags on either side of a positional and both readings are reasonable, so
// accept both rather than making the order load-bearing.
func parseInterspersed(flags *flag.FlagSet, arguments []string) ([]string, error) {
	positional := []string{}
	for {
		if err := flags.Parse(arguments); err != nil {
			return nil, err
		}
		remaining := flags.Args()
		if len(remaining) == 0 {
			return positional, nil
		}
		positional = append(positional, remaining[0])
		arguments = remaining[1:]
	}
}
