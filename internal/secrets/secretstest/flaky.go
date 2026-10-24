package secretstest

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/secrets"
)

// flakyWrapper delegates to a real wrapper and fails Wrap or Unwrap on demand.
type flakyWrapper struct {
	mu        sync.Mutex
	inner     secrets.KeyWrapper
	wrapErr   error
	unwrapErr error
}

func (f *flakyWrapper) setWrap(err error)   { f.mu.Lock(); f.wrapErr = err; f.mu.Unlock() }
func (f *flakyWrapper) setUnwrap(err error) { f.mu.Lock(); f.unwrapErr = err; f.mu.Unlock() }

func (f *flakyWrapper) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	f.mu.Lock()
	err := f.wrapErr
	f.mu.Unlock()
	if err != nil {
		return secrets.WrappedKey{}, err
	}
	return f.inner.Wrap(ctx, wc, key)
}

func (f *flakyWrapper) Unwrap(ctx context.Context, wc secrets.WrapContext, w secrets.WrappedKey) ([]byte, error) {
	f.mu.Lock()
	err := f.unwrapErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.inner.Unwrap(ctx, wc, w)
}
