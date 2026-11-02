package dispatch_test

import (
	"context"
	"errors"
	"runtime"
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

// errPastNameValidation is what the probe reports when Subscribe accepted the name.
var errPastNameValidation = errors.New("past name validation")

// subscribeValidatesName exercises only the local name validation of
// natsbus.JetStream.Subscribe; it says nothing about a broker accepting the
// consumer. Subscribe checks the name before it touches the stream, so on a zero
// adapter (no stream) a name that passes faults at its first use of the nil
// stream, while a name that fails is returned as an error first. Only that
// nil-pointer dereference counts as "passed validation": any other panic is
// re-raised so an unrelated failure cannot read as success.
func subscribeValidatesName(t *testing.T, name string) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			var re runtime.Error
			if e, ok := r.(error); ok && errors.As(e, &re) && strings.Contains(re.Error(), "nil pointer dereference") {
				err = errPastNameValidation
				return
			}
			panic(r)
		}
	}()
	_, err = (&natsbus.JetStream{}).Subscribe(t.Context(), name, dispatch.Filter, func(context.Context, bus.Message) error { return nil })
	if err == nil {
		t.Fatalf("Subscribe on an adapter without a stream returned no error for %q", name)
	}
	return err
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

	// Subscribe's local name validation passes it ...
	if err := subscribeValidatesName(t, name); !errors.Is(err, errPastNameValidation) {
		t.Fatalf("Subscribe's name validation refused %q: %v", name, err)
	}
	// ... and refuses an out-of-scheme name, so the probe discriminates. Whether a
	// broker accepts the consumer is outside this unit test.
	if err := subscribeValidatesName(t, "Not A.Name"); err == nil || errors.Is(err, errPastNameValidation) {
		t.Fatalf("Subscribe's name validation passed an out-of-scheme name: %v", err)
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
