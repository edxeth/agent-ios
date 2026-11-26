package webkit

import "context"

// noLivenessProbeKey is the context key of WithoutLivenessProbe.
type noLivenessProbeKey struct{}

// WithoutLivenessProbe marks ctx so that a call made with it, or with a
// context derived from it, that gets no reply runs no liveness probe of
// the page connection (core's pageConn, design 2.6) and only fails. It is
// for a call whose missing reply says nothing about the page: a
// best-effort read after the command's work succeeded, or the armed await
// of a runtime call, which its sender cancels when the call ends without
// needing it.
func WithoutLivenessProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, noLivenessProbeKey{}, true)
}

// LivenessProbeOff reports whether ctx was marked by WithoutLivenessProbe.
func LivenessProbeOff(ctx context.Context) bool {
	return ctx.Value(noLivenessProbeKey{}) != nil
}
