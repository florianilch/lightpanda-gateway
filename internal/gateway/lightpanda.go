package gateway

import (
	"maps"
	"os"
)

// maxProcessTailBytes limits the diagnostic tail kept from each managed Lightpanda
// child-process stream. Copying continues after the limit.
const maxProcessTailBytes = 32 << 10

// lightpandaExtraEnv returns a copy of extraEnv. When set in the lpgw environment,
// LIGHTPANDA_DISABLE_TELEMETRY is added to the returned map.
func lightpandaExtraEnv(extraEnv map[string]string) map[string]string {
	env := maps.Clone(extraEnv)
	if value, ok := os.LookupEnv("LIGHTPANDA_DISABLE_TELEMETRY"); ok {
		if env == nil {
			env = make(map[string]string, 1)
		}
		env["LIGHTPANDA_DISABLE_TELEMETRY"] = value
	}
	return env
}
