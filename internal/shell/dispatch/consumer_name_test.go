package dispatch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/dispatch"
)

// recordingSubscriber captures the durable name and filter a consumer attaches with.
type recordingSubscriber struct{ name, filter string }

func (r *recordingSubscriber) Subscribe(_ context.Context, name, filter string, _ bus.Handler) (func(), error) {
	r.name, r.filter = name, filter
	return func() {}, nil
}

// subscribeReachesBroker reports whether natsbus.JetStream.Subscribe accepted the
// name: it validates the name before it touches the stream, so a name that passes
// reaches the (here absent) broker and faults on the zero adapter, while a name
// that fails is returned as a validation error without reaching it.
func subscribeReachesBroker(t *testing.T, name string) (reached bool, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			reached, err = true, nil
		}
	}()
	_, err = (&natsbus.JetStream{}).Subscribe(t.Context(), name, dispatch.Filter, func(context.Context, bus.Message) error { return nil })
	return false, err
}

func Test_unit_dispatch_durable_consumer_name(t *testing.T) {
	name := dispatch.Consumer

	// It is exactly what the shared scheme produces for a (service, purpose) pair.
	service, purpose, err := natsbus.ParseConsumerName(name)
	if err != nil {
		t.Fatalf("%q is not a name of the scheme: %v", name, err)
	}
	if built, err := natsbus.ConsumerName(service, purpose); err != nil || built != name {
		t.Fatalf("ConsumerName(%q, %q) = %q, %v; want %q", service, purpose, built, err, name)
	}
	if !strings.HasPrefix(name, "kodama_") || service != "shell" || purpose != "git-dispatch" {
		t.Fatalf("name = %q (service %q, purpose %q)", name, service, purpose)
	}

	// Subscribe accepts it ...
	if reached, err := subscribeReachesBroker(t, name); !reached || err != nil {
		t.Fatalf("Subscribe refused %q: reached=%v err=%v", name, reached, err)
	}
	// ... and does refuse an out-of-scheme name, so the probe discriminates.
	if reached, err := subscribeReachesBroker(t, "Not A.Name"); reached || err == nil {
		t.Fatalf("Subscribe accepted an out-of-scheme name: reached=%v err=%v", reached, err)
	}

	// It is not the retired literal, which the scheme does not produce.
	if name == dispatch.RetiredConsumer || dispatch.RetiredConsumer != "kernel-git-dispatch" {
		t.Fatalf("name %q must differ from the retired %q", name, dispatch.RetiredConsumer)
	}
	if _, _, err := natsbus.ParseConsumerName(dispatch.RetiredConsumer); err == nil {
		t.Fatalf("the retired literal %q is a name of the scheme", dispatch.RetiredConsumer)
	}

	// The dispatcher attaches under that name and its filter.
	d, err := dispatch.New(dispatch.Config{Store: memStore{}, Files: filesFunc(nil)})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingSubscriber{}
	if _, err := d.Subscribe(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if rec.name != name || rec.filter != dispatch.Filter {
		t.Fatalf("subscribed as %q on %q; want %q on %q", rec.name, rec.filter, name, dispatch.Filter)
	}
}
