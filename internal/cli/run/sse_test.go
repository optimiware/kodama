package run

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func frames(t *testing.T, input string, max int) ([]Frame, error) {
	t.Helper()
	fr := newFrameReader(strings.NewReader(input), max)
	var out []Frame
	for {
		f, err := fr.next()
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

func Test_sse_frame_reader(t *testing.T) {
	in := "\xef\xbb\xbfevent: snapshot\r\nid: c1\r\ndata: {\"a\":1}\r\n\r\n" +
		": heartbeat\n\n" +
		"event: run.state_changed\nid: c2\ndata: line1\ndata: line2\n\n" +
		"retry: 100\nunknown: x\nevent: checkpoint\nid: c3\ndata: {}\n\n" +
		"\n\n" + // stray blank lines dispatch nothing
		"data: bare\n\n" +
		"event: resync_required\ndata: {}\n\n"
	got, err := frames(t, in, 1<<10)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err %v", err)
	}
	want := []struct {
		kind  FrameKind
		event string
		id    string
		data  string
	}{
		{FrameSnapshot, "snapshot", "c1", `{"a":1}`},
		{FrameHeartbeat, "", "", ""},
		{FrameEvent, "run.state_changed", "c2", "line1\nline2"},
		{FrameCheckpoint, "checkpoint", "c3", "{}"},
		{FrameEvent, "message", "", "bare"},
		{FrameResync, "resync_required", "", "{}"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d frames: %+v", len(got), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.kind || g.Event != w.event || g.ID != w.id || string(g.Data) != w.data {
			t.Errorf("frame %d: %+v, want %+v", i, g, w)
		}
	}
}

func Test_sse_partial_and_oversized_frames_are_never_delivered(t *testing.T) {
	got, err := frames(t, "event: a\nid: c1\ndata: {}\n\nevent: b\nid: c2\ndata: {", 1<<10)
	if len(got) != 1 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("%d frames, err %v: a cut-off frame must surface as an unexpected EOF", len(got), err)
	}
	got, err = frames(t, "event: a\nid: c1\ndata: {}\n\n", 1<<10)
	if len(got) != 1 || !errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("clean end: %d frames, err %v", len(got), err)
	}
	_, err = frames(t, "event: a\ndata: "+strings.Repeat("x", 200)+"\n\n", 64)
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("err %v", err)
	}
	// Many small lines add up to an oversized frame too.
	_, err = frames(t, "event: a\n"+strings.Repeat("data: xxxxxxxx\n", 40)+"\n", 128)
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("accumulated: err %v", err)
	}
	_, err = frames(t, "id: a\x00b\ndata: {}\n\n", 1<<10)
	if !errors.Is(err, errBadFrame) {
		t.Fatalf("nul in id: err %v", err)
	}
}
