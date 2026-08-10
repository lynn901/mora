package outbox

import (
	"context"
	"errors"
	"testing"
)

// fakePublisher is a hand-rolled StreamPublisher used by dispatcher tests.
type fakePublisher struct {
	published map[string][][]byte // stream -> payloads (order preserved)
	idCounter int
	err       error // force a failure on the next Publish
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{published: map[string][][]byte{}}
}

func (f *fakePublisher) Publish(_ context.Context, stream string, payload []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.published[stream] = append(f.published[stream], payload)
	f.idCounter++
	return "fake-id", nil
}

// TestValkeyPublisher_NilClientRejected: a publisher with no client refuses to
// publish (no silent nil-deref). §6.3: a publish failure must surface as an
// error so the dispatcher records the delivery attempt and retries.
func TestValkeyPublisher_NilClientRejected(t *testing.T) {
	p := NewValkeyPublisher(nil, 1000)
	_, err := p.Publish(context.Background(), "knowledge_events", []byte("{}"))
	if err == nil {
		t.Fatal("nil client must return an error, not publish")
	}
}

// TestValkeyPublisher_InterfaceSatisfied: compile-time guarantee that the
// concrete type fulfills StreamPublisher. Re-asserted here so a refactor can't
// silently break the contract (the ValkeyPublisher is wired into the dispatcher
// under this exact interface).
func TestValkeyPublisher_InterfaceSatisfied(t *testing.T) {
	var _ StreamPublisher = (*ValkeyPublisher)(nil)
	var _ StreamPublisher = (*fakePublisher)(nil)
}

// TestNewDispatcher_Defaults: zero/negative batch + interval fall back to the
// documented defaults so a misconfigured caller can't starve the poller or
// busy-loop.
func TestNewDispatcher_Defaults(t *testing.T) {
	d := NewDispatcher(nil, nil, 0, 0)
	if d.batch != 50 {
		t.Errorf("default batch = %d, want 50", d.batch)
	}
	if d.interval <= 0 {
		t.Errorf("default interval = %v, want > 0", d.interval)
	}
}

// TestNewDispatcher_RespectsExplicitValues: a caller that sets explicit batch
// and interval gets exactly those, not the defaults.
func TestNewDispatcher_RespectsExplicitValues(t *testing.T) {
	d := NewDispatcher(nil, nil, 25, 42)
	if d.batch != 25 {
		t.Errorf("batch = %d, want 25", d.batch)
	}
	if d.interval != 42 {
		t.Errorf("interval = %v, want 42", d.interval)
	}
}

// TestNewDispatcher_StreamsMapAccessible: the configured stream->publisher map
// is reachable so a dispatcher with a publisher for knowledge_events can route
// to it. Full routing (deliver -> publish -> mark published) is exercised by
// the integration test against a live Postgres.
func TestNewDispatcher_StreamsMapAccessible(t *testing.T) {
	pub := newFakePublisher()
	d := NewDispatcher(nil, map[string]StreamPublisher{"knowledge_events": pub}, 10, 1)
	got, ok := d.streams["knowledge_events"]
	if !ok {
		t.Fatal("expected knowledge_events publisher to be registered")
	}
	if got != pub {
		t.Fatal("registered publisher mismatch")
	}
}

// TestFakePublisher_RecordsPayloads: the fake publisher records exactly the
// payloads it receives, in order — so dispatcher tests can assert what hit each
// stream without a live Valkey.
func TestFakePublisher_RecordsPayloads(t *testing.T) {
	pub := newFakePublisher()
	_, _ = pub.Publish(context.Background(), "knowledge_events", []byte("a"))
	_, _ = pub.Publish(context.Background(), "knowledge_events", []byte("b"))
	got := pub.published["knowledge_events"]
	if len(got) != 2 || string(got[0]) != "a" || string(got[1]) != "b" {
		t.Fatalf("recorded payloads = %v, want [a b]", got)
	}
}

// TestFakePublisher_SurfacesError: a forced error propagates and no payload is
// recorded — so dispatcher deliver() treats it as a failed delivery (records
// last_error, leaves the event unpublished for retry).
func TestFakePublisher_SurfacesError(t *testing.T) {
	pub := newFakePublisher()
	pub.err = errors.New("valkey down")
	_, err := pub.Publish(context.Background(), "knowledge_events", []byte("a"))
	if err == nil {
		t.Fatal("forced error must propagate")
	}
	if len(pub.published["knowledge_events"]) != 0 {
		t.Fatal("failed publish must not record the payload")
	}
}
