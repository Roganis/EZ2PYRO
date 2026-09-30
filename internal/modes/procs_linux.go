//go:build linux

package modes

import (
	"os"
	"strings"
)

// runningProcesses lists process names from /proc/*/comm.
func runningProcesses() []string {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if c := e.Name()[0]; c < '0' || c > '9' {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err == nil {
			out = append(out, strings.TrimSpace(string(b)))
		}
	}
	return out
}
