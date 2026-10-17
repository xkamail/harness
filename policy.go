package harness

// Policy is how conversations of an agent are opened.
type Policy int

const (
	// PolicyNew starts a new conversation on every start.
	PolicyNew Policy = iota
	// PolicyPerContext keeps one conversation per (owner, agent, Values) and
	// resumes it.
	PolicyPerContext
	// PolicyShared keeps one conversation per (agent, Values) that several
	// principals write to, such as a support thread.
	PolicyShared
)

func (p Policy) String() string {
	switch p {
	case PolicyPerContext:
		return "per_context"
	case PolicyShared:
		return "shared"
	default:
		return "new"
	}
}

// ModelPolicy is when a conversation may change model.
type ModelPolicy int

const (
	// ModelPinned picks the model when the conversation starts and keeps it.
	ModelPinned ModelPolicy = iota
	// ModelPerRun picks the model again at the start of every run, never
	// within one.
	ModelPerRun
)

func (p ModelPolicy) String() string {
	if p == ModelPerRun {
		return "per_run"
	}
	return "pinned"
}
