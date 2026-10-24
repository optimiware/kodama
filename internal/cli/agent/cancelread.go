package agent

import (
	"context"
	"errors"
	"io"
	"time"
)

// readBounded reads at most max+1 bytes from r so a caller can tell an oversize
// input from one that fits exactly. The read honours ctx: a redirected pipe
// that never produces data cannot outlast --timeout or an interrupt.
//
// The read runs in a goroutine that only ever hands its bytes back through a
// buffered channel. After cancellation nothing consumes the result except a
// cleaner that clears it, so an abandoned reader cannot contribute input to a
// request that was never sent. wake, when set, interrupts the blocked read
// (closing a handle this package opened); a file that cannot take a read
// deadline is left to finish on its own and the process ends with the command.
func readBounded(ctx context.Context, r io.Reader, max int64, wake func()) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, max+1))
		done <- outcome{b, err}
	}()
	select {
	case o := <-done:
		if err := ctx.Err(); err != nil && o.err == nil {
			// Cancelled while the last bytes arrived: the command was cut off,
			// so the input is not used.
			clear(o.data)
			return nil, err
		}
		if o.err != nil {
			clear(o.data)
			return nil, o.err
		}
		return o.data, nil
	case <-ctx.Done():
		interrupt(r, wake)
		go func() { clear((<-done).data) }()
		return nil, ctx.Err()
	}
}

// interrupt wakes a blocked read where the handle supports it.
func interrupt(r io.Reader, wake func()) {
	if wake != nil {
		wake()
	}
	if d, ok := r.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now())
	}
}

// isContextError reports whether err is a cancellation or deadline.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
