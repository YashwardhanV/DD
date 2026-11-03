package trace

import "context"

type ctxKey struct{}

// WithRecorder returns a context that carries r, so planning code deep inside
// the copier (listing, checking the destination) can time itself without
// every function growing a parameter. A nil r is allowed.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// From returns the Recorder carried by ctx, or nil.
func From(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(ctxKey{}).(*Recorder)
	return r
}
