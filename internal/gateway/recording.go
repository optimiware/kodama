package gateway

import (
	"context"
	"slices"
)

// frameRecorder collects the validated stream frames of one request for the
// optional content recorder, within a byte bound. It exists only when the
// request's effective recording decision is on; with recording off no frame is
// ever copied, kept or handed to anything.
type frameRecorder struct {
	max       int64
	n         int64
	frames    [][]byte
	truncated bool
}

func newFrameRecorder(max int64) *frameRecorder { return &frameRecorder{max: max} }

func (r *frameRecorder) add(frame []byte) {
	if r == nil || r.truncated {
		return
	}
	if r.n+int64(len(frame)) > r.max {
		r.truncated = true
		return
	}
	r.n += int64(len(frame))
	r.frames = append(r.frames, slices.Clone(frame))
}

// record hands the exchange to the recorder and stores the attributable
// outcome. A recorder failure never changes the workload's result or the usage
// record; it is visible on the request (recording_outcome = failed) and in the
// returned flag. Both calls use a bounded context that survives the caller's
// cancellation, so a disconnect cannot hide a recording failure.
func (s *Service) record(ctx context.Context, w Workload, id RequestID, c Completion, fr *frameRecorder) (failed bool) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeouts.Record)
	defer cancel()
	err := s.cfg.Recorder.Record(cctx, Exchange{
		Scope: w.Scope, RunID: w.RunID, AttemptID: w.AttemptID, RequestID: id,
		Completion: c, Frames: fr.frames, Truncated: fr.truncated,
	})
	if err != nil {
		s.cfg.Logger.Warn("content recording failed", "requestId", string(id), "attemptId", string(w.AttemptID))
	}
	if serr := s.cfg.Store.RecordingOutcome(cctx, w.Scope, id, err == nil); serr != nil {
		s.cfg.Logger.Warn("recording outcome was not stored", "requestId", string(id))
	}
	return err != nil
}
