//go:build !linux && !windows

package telemetry

// Available returns the collectors this platform supports (none yet).
func Available(o Options) []Collector { return nil }
