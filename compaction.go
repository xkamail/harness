package harness

import "context"

// CompactionConfig is when and how old history is summarized.
type CompactionConfig struct {
	// Threshold compacts once the last run's ContextTokens reach it.
	Threshold int64
	// KeepTail is how many recent user turns stay verbatim; zero means 4.
	KeepTail int
	// Model summarizes; nil uses the run's model.
	Model Model
	// MaxSummaryTokens caps the summary; a summary cut off at the cap is
	// rejected and the history kept as it was.
	MaxSummaryTokens int64
}

// Compaction is a summary that replaces the history up to Through.
type Compaction struct {
	Summary string
	// Through is the index in the input history of the last message the
	// summary replaces.
	Through int
	Usage   Usage
}

// Compact summarizes history before a cut at a user-message boundary,
// keeping cfg.KeepTail user turns verbatim. It never splits an assistant
// turn from its tool results. It returns nil, nil when there is nothing
// worth compacting, and an error, history untouched, when the summary
// fails or is cut off.
func Compact(ctx context.Context, m Model, history []Message, cfg CompactionConfig) (*Compaction, error) {
	// TODO: port from mccloud api/ai/summary.go, with the rules above.
	return nil, ErrNotImplemented
}
