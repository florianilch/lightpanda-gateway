package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// envParseError records an invalid environment value and the flag that can
// override it on the command line.
type envParseError struct {
	flagName string
	err      error
}

// applyEnvDefaults applies environment values for every flag except version. For
// every flag except browser-launch-arg, the environment name is LPGW_, then the
// upper-case flag name with '-' replaced by '_'. browser-launch-arg uses
// LPGW_BROWSER_LAUNCH_ARGS as a JSON string array. When unset, it adds no browser
// launch arguments; [] is valid and supplies no arguments.
//
// A valid value becomes the default shown in usage. An invalid environment value
// returns an error.
func applyEnvDefaults(fs *flag.FlagSet) []envParseError {
	var errs []envParseError

	fs.VisitAll(func(f *flag.Flag) {
		switch f.Name {
		case "version":
			return
		case "browser-launch-arg":
			const envName = "LPGW_BROWSER_LAUNCH_ARGS"
			value, ok := os.LookupEnv(envName)
			if !ok {
				return
			}
			var args []string
			if err := json.Unmarshal([]byte(value), &args); err != nil {
				errs = append(errs, envParseError{
					flagName: f.Name,
					err:      fmt.Errorf("config: invalid value %q for %s: must be a JSON array of strings: %w", value, envName, err),
				})
				return
			}
			if args == nil {
				errs = append(errs, envParseError{
					flagName: f.Name,
					err:      fmt.Errorf("config: invalid value %q for %s: must be a JSON array of strings", value, envName),
				})
				return
			}
			for _, arg := range args {
				if err := f.Value.Set(arg); err != nil {
					errs = append(errs, envParseError{
						flagName: f.Name,
						err:      fmt.Errorf("config: invalid value %q for %s: %w", value, envName, err),
					})
					return
				}
			}
			f.DefValue = value
			return
		}

		envName := "LPGW_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		value, ok := os.LookupEnv(envName)
		if !ok {
			return
		}

		if err := f.Value.Set(value); err != nil {
			errs = append(errs, envParseError{
				flagName: f.Name,
				err:      fmt.Errorf("config: invalid value %q for %s: %w", value, envName, err),
			})
			return
		}
		f.DefValue = f.Value.String()
	})

	return errs
}
