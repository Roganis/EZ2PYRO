// Package router defines the optional router-side telemetry source. The
// Freebox implementation lives in router/freebox; others can be added behind
// the same interface.
package router

import (
	"context"

	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Router samples the router's view of the Wi-Fi network. It is read-only.
type Router interface {
	// Connect authenticates (possibly asking the user to approve the app on
	// the router the first time).
	Connect(ctx context.Context) error
	// Sample returns the current router-side samples (KindRouter samples and
	// router events such as channel changes). Times are filled in by the caller.
	Sample(ctx context.Context) ([]telemetry.Sample, error)
	Close() error
}
