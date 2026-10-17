package harness

// Decorators are how an Agent is extended: plain functions from Agent to
// Agent, applied before Compile.
//
//	agents := map[string]*harness.Agent{
//		console.Name: harness.MustCompile(withTodos(withWatches(console.Agent()))),
//	}

// WrapHandlers returns a with every tool's Handler and Check passed through
// wrap, such as to mask raw errors or log calls. It copies Tools, so a is
// not modified.
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

// AddTools returns a with tools appended. It copies Tools.
func AddTools(a Agent, tools ...Tool) Agent {
	a.Tools = append(append([]Tool(nil), a.Tools...), tools...)
	return a
}
