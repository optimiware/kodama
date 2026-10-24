package logshttp

import (
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/logs"
)

func toMeta(m logs.Meta) LogMeta {
	reasons := make([]LogReason, 0, len(m.Reasons))
	for _, r := range m.Reasons {
		reasons = append(reasons, LogReason(r))
	}
	return LogMeta{
		Type: Meta, RunId: httpapi.ID(m.Run), AttemptId: httpapi.ID(m.Attempt), StepId: m.Step,
		Origin: LogOrigin(m.Origin), Completeness: LogCompleteness(m.Completeness), Reasons: reasons,
		Tail: m.Tail, Follow: m.Follow, Terminal: m.Terminal,
		Limits: LogLimits{MaxBytes: int(m.Limits.MaxBytes), MaxLines: m.Limits.MaxLines, MaxLineBytes: m.Limits.MaxLineBytes, MaxDurationSeconds: int(m.Limits.MaxDurationSecs)},
	}
}

func toChunk(c logs.Chunk) LogChunk {
	return LogChunk{Type: LogChunkTypeChunk, Text: c.Text, LineTruncated: c.LineTruncated}
}

func toSummary(s logs.Summary) LogSummary {
	return LogSummary{Type: Summary, Lines: s.Lines, Bytes: int(s.Bytes), Truncated: s.Truncated, Stop: LogStop(s.Stop)}
}
