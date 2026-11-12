package native

import "context"

type commandEnvironmentKey struct{}

// WithCommandEnvironment attaches the receive-pack quarantine object
// environment to a context. The app executor appends it to the child
// environment so the in-process ancestry check can see the pushed objects.
func WithCommandEnvironment(ctx context.Context, environment []string) context.Context {
	if len(environment) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	copied := append([]string(nil), environment...)
	return context.WithValue(ctx, commandEnvironmentKey{}, copied)
}

// CommandEnvironment returns the context-carried command environment.
func CommandEnvironment(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	environment, _ := ctx.Value(commandEnvironmentKey{}).([]string)
	return environment
}

// WithoutCommandEnvironment detaches any receive-pack quarantine environment
// from ctx. A read of a different repository (for example the trusted admin
// policy while a tenant repository push is being verified) must see that
// repository's own objects, never the pushed repository's quarantine.
func WithoutCommandEnvironment(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, commandEnvironmentKey{}, []string{})
}
