package main

import (
	"fmt"
	"strings"
)

type cliOptions struct {
	version       bool
	configPath    string
	http          bool
	httpAddr      string
	httpStateless bool
}

func parseCLI(args []string) (cliOptions, error) {
	var o cliOptions
	seen := map[string]bool{}
	once := func(flag string) error {
		if seen[flag] {
			return fmt.Errorf("%s given more than once", flag)
		}
		seen[flag] = true
		return nil
	}
	value := func(i int, flag string) (string, error) {
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := strings.Cut(a, "=")
		switch name {
		case "--version", "-v":
			if hasInline {
				return cliOptions{}, fmt.Errorf("%s takes no value", name)
			}
			o.version = true
		case "--config":
			if err := once(name); err != nil {
				return cliOptions{}, err
			}
			if hasInline {
				if inline == "" {
					return cliOptions{}, fmt.Errorf("--config needs a value")
				}
				o.configPath = inline
				continue
			}
			v, err := value(i, name)
			if err != nil {
				return cliOptions{}, err
			}
			o.configPath = v
			i++
		case "--http":
			if err := once(name); err != nil {
				return cliOptions{}, err
			}
			o.http = true
			switch {
			case hasInline && inline == "":
				return cliOptions{}, fmt.Errorf("--http= needs an address")
			case hasInline:
				o.httpAddr = inline
			case i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
				o.httpAddr = args[i+1]
				i++
			}
		case "--http-stateless":
			if hasInline {
				return cliOptions{}, fmt.Errorf("%s takes no value", name)
			}
			if err := once(name); err != nil {
				return cliOptions{}, err
			}
			o.httpStateless = true
		default:
			return cliOptions{}, fmt.Errorf("unknown argument %q (known: --config <path>, --http [addr], --http-stateless, --version)", a)
		}
	}
	return o, nil
}
