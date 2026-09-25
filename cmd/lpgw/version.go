package main

import (
	"fmt"
	"runtime/debug"
)

func printVersion() {
	version, revision := versionInfo()
	switch {
	case version != "" && revision != "":
		fmt.Printf("lpgw version %s, revision %s\n", version, revision)
	case version != "":
		fmt.Printf("lpgw version %s\n", version)
	default:
		fmt.Println("lpgw version")
	}
}

// versionInfo returns the version and VCS revision stamped by the Go toolchain.
// Missing build information produces empty values.
func versionInfo() (version, revision string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}

	version = info.Main.Version
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			revision = setting.Value
			break
		}
	}
	return version, revision
}
