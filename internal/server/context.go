package server

import "context"

// mergeCancel returns a context derived from ctx that is also cancelled when
// other is done.
func mergeCancel(ctx, other context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(other, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
