package harness

// Policy is how conversations of an agent are opened.
type Policy int

const (
	// PolicyNew starts a new conversation on every start.
	PolicyNew Policy = iota
	// PolicyPerSubject keeps one conversation per (owner, agent, scope) and
	// resumes it.
	PolicyPerSubject
	// PolicyShared keeps one conversation per (agent, scope) that several
	// principals write to, such as a support thread.
	PolicyShared
)

func (p Policy) String() string {
	switch p {
	case PolicyPerSubject:
		return "per_subject"
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
