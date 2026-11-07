package core

import "context"

type noTransportStartKey struct{}

// WithoutTransportStart marks ctx so that a TransportFunc does not start a
// transport process (iwdp, decision D10) for this call: reading a listing to
// classify an explicit page URL must not start iwdp as a side effect.
func WithoutTransportStart(ctx context.Context) context.Context {
	return context.WithValue(ctx, noTransportStartKey{}, true)
}

// TransportStartAllowed reports whether a TransportFunc may start a
// transport process for ctx.
func TransportStartAllowed(ctx context.Context) bool {
	return ctx.Value(noTransportStartKey{}) == nil
}
