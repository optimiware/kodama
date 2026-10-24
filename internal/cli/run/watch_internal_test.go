package run

import (
	"context"
	"io"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
)

type scripted struct {
	frames []Frame
	i      int
}

func (s *scripted) Next() (Frame, error) {
	if s.i >= len(s.frames) {
		return Frame{}, io.EOF
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}
func (s *scripted) Close() error { return nil }

func newTestWatcher() *watcher {
	return &watcher{scope: client.Scope{Tenant: "t_acme", Project: "p_inventory"}, runID: "run_1", seen: map[string]struct{}{}}
}

const (
	snapData = `{"cursor":"c1.s","resources":[{"type":"run","id":"run_1","version":2,"state":"running"}]}`
	goodEvt  = `{"id":"evt_2","type":"run.state_changed","tenantId":"t_acme","resource":{"type":"run","id":"run_1","version":3},"data":{"to":"running"}}`
)

func snapFrame() Frame {
	return Frame{Kind: FrameSnapshot, Event: "snapshot", ID: "c1.s", Data: []byte(snapData)}
}

// A frame that cannot be applied leaves the accepted cursor, the deduplication
// state and the applied run version exactly where the last accepted frame left
// them.
func Test_watch_rejected_frames_do_not_advance_state(t *testing.T) {
	bad := map[string]Frame{
		"event without version": {Kind: FrameEvent, Event: "run.state_changed", ID: "c2.s",
			Data: []byte(`{"id":"evt_2","type":"run.state_changed","tenantId":"t_acme","resource":{"type":"run","id":"run_1"},"data":{"to":"succeeded"}}`)},
		"event without target": {Kind: FrameEvent, Event: "run.state_changed", ID: "c2.s",
			Data: []byte(`{"id":"evt_2","type":"run.state_changed","tenantId":"t_acme","resource":{"type":"run","id":"run_1","version":3},"data":{}}`)},
		"event with bad cursor": {Kind: FrameEvent, Event: "run.state_changed", ID: "nope",
			Data: []byte(goodEvt)},
		"checkpoint without cursor": {Kind: FrameCheckpoint, Event: "checkpoint", ID: "c2.s", Data: []byte(`{}`)},
		"checkpoint bad cursor":     {Kind: FrameCheckpoint, Event: "checkpoint", ID: "bad cursor", Data: []byte(`{"cursor":"bad cursor"}`)},
		"resync bad payload":        {Kind: FrameResync, Event: "resync_required", Data: []byte(`{"code":"x"}`)},
	}
	for name, f := range bad {
		t.Run(name, func(t *testing.T) {
			w := newTestWatcher()
			_, resync, err := w.consume(context.Background(), &scripted{frames: []Frame{snapFrame(), f}})
			if err == nil || resync {
				t.Fatalf("err %v resync %v: the frame must be refused", err, resync)
			}
			if w.cursor != "c1.s" || w.version != 2 || w.status != "running" || len(w.seen) != 0 || w.stats.Events != 0 {
				t.Fatalf("state moved: cursor %q version %d status %q seen %d events %d", w.cursor, w.version, w.status, len(w.seen), w.stats.Events)
			}
		})
	}
	t.Run("an accepted event advances all of it", func(t *testing.T) {
		w := newTestWatcher()
		_, _, err := w.consume(context.Background(), &scripted{frames: []Frame{snapFrame(),
			{Kind: FrameEvent, Event: "run.state_changed", ID: "c2.s", Data: []byte(goodEvt)}}})
		if err != io.EOF && err != nil {
			t.Fatal(err)
		}
		if w.cursor != "c2.s" || w.version != 3 || len(w.seen) != 1 || w.stats.Events != 1 {
			t.Fatalf("cursor %q version %d seen %d", w.cursor, w.version, len(w.seen))
		}
	})
}

func Test_validators(t *testing.T) {
	if _, ok := decodeSnapshot([]byte(snapData)); !ok {
		t.Fatal("valid snapshot refused")
	}
	if _, ok := decodeSnapshot([]byte(`{"cursor":"c1.s","resources":[],"future":1}`)); !ok {
		t.Fatal("additive members must be accepted")
	}
	for _, c := range []string{"", "a", "a.b.c", "a b.c", "a.b\n", string(make([]byte, 3000))} {
		if validCursor(c) {
			t.Errorf("cursor %q accepted", c)
		}
	}
}
