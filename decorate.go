package harness

// WrapHandlers wraps every tool's Handler and Check; a.Tools is copied, not mutated.
func WrapHandlers(a Agent, wrap func(t Tool, h Handler) Handler) Agent {
	tools := make([]Tool, len(a.Tools))
	copy(tools, a.Tools)
	for i := range tools {
		if tools[i].Handler != nil {
			tools[i].Handler = wrap(tools[i], tools[i].Handler)
		}
		if tools[i].Check != nil {
			tools[i].Check = wrap(tools[i], tools[i].Check)
		}
	}
	a.Tools = tools
	return a
}

// AddTools appends to a copy of a.Tools.
func AddTools(a Agent, tools ...Tool) Agent {
	a.Tools = append(append([]Tool(nil), a.Tools...), tools...)
	return a
}
