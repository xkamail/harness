// Command sse serves one shared conversation to a web UI over Server-Sent Events.
//
//	OPENAI_API_KEY=sk-... go run ./cmd/sse -model gpt-5-mini   (or -fake)
//
// Persisted events get a Seq; ephemeral ones (deltas, tool_start, tool_done) only fan out.
// POST /api/messages during a run queues user.queued for the run's Inbox (steering).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/harnesstest"
	"github.com/xkamail/harness/openaiharness"
)

//go:embed index.html
var indexHTML []byte

var agent = harness.MustCompile(harness.Agent{
	Name:         "sse",
	SystemPrompt: "You are a terse ops assistant. Use tools when they help.",
	Tools: []harness.Tool{
		{
			Name:        "now",
			Description: "Call when you need the current time.",
			Label:       "Checking the time",
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return time.Now().Format(time.RFC3339), nil
			},
		},
		{
			Name:        "restart_service",
			Description: "Call to restart a service by name.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}},"required":["service"]}`),
			Tier:        harness.TierDisruptive,
			GrantGroup:  "ops",
			Label:       "Restarting service",
			Describe:    func(args json.RawMessage) string { return "Restart " + service(args) },
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				time.Sleep(2 * time.Second) // slow enough to watch tool_start → tool_done
				return "restarted " + service(args), nil
			},
		},
		{
			Name:        "delete_volume",
			Description: "Call to delete a service's data volume.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}},"required":["service"]}`),
			Tier:        harness.TierDestructive,
			Label:       "Deleting volume",
			Describe:    func(args json.RawMessage) string { return "Delete the volume of " + service(args) },
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				return "pretended to delete the volume of " + service(args), nil
			},
		},
	},
})

func service(args json.RawMessage) string {
	var in struct{ Service string }
	_ = json.Unmarshal(args, &in)
	return in.Service
}

func main() {
	var (
		addr    = flag.String("addr", ":8080", "listen address")
		modelID = flag.String("model", "gpt-5-mini", "model id")
		baseURL = flag.String("base-url", "", "OpenAI-compatible endpoint; empty is OpenAI")
		fake    = flag.Bool("fake", false, "use a scripted model, no network")
	)
	flag.Parse()

	c := &convo{model: newModel(*modelID, *baseURL, *fake), subs: map[chan harness.Event]struct{}{}, status: harness.StatusIdle}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/events", c.serveEvents)
	mux.HandleFunc("POST /api/messages", c.postMessage)
	mux.HandleFunc("POST /api/decision", c.postDecision)
	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// convo keeps in memory what a project keeps in its database and bus.
type convo struct {
	model harness.Model

	mu     sync.Mutex
	events []harness.Event // persisted; events[i].Seq == i+1
	subs   map[chan harness.Event]struct{}

	history []harness.Message
	grants  harness.Grants
	status  harness.Status
	runs    int
	held    *harness.ToolCall // the open confirmation or plan
	queued  []queuedMsg       // steering messages the run has not read yet
	taken   []int64           // Seq of queued messages the run read, in order
}

type queuedMsg struct {
	seq  int64
	text string
}

// QueuedSeq ties a delivered message to the user.queued it was sent as.
type userPayload struct {
	Text      string `json:"text"`
	QueuedSeq int64  `json:"queuedSeq,omitempty"`
}

func newEvent(typ harness.EventType, runID string, payload any) harness.Event {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return harness.Event{Type: typ, RunID: runID, Payload: b, CreatedAt: time.Now()}
}

// emitLocked drops a slow subscriber; its EventSource reconnects and catches up
// from Last-Event-ID.
func (c *convo) emitLocked(ev harness.Event) {
	if !ev.Type.Ephemeral() {
		ev.Seq = int64(len(c.events)) + 1
		c.events = append(c.events, ev)
	}
	for ch := range c.subs {
		select {
		case ch <- ev:
		default:
			delete(c.subs, ch)
			close(ch)
		}
	}
}

func (c *convo) emit(ev harness.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.emitLocked(ev)
}

// serveEvents replays after Last-Event-ID (or ?after=), then streams live; backlog
// and subscription share one lock so no event is missed or sent twice.
func (c *convo) serveEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if after == 0 {
		after, _ = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := make(chan harness.Event, 256)
	c.mu.Lock()
	backlog := append([]harness.Event(nil), c.events[min(after, int64(len(c.events))):]...)
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if _, ok := c.subs[ch]; ok {
			delete(c.subs, ch)
			close(ch)
		}
		c.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	write := func(ev harness.Event) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if ev.Seq > 0 {
			fmt.Fprintf(w, "id: %d\n", ev.Seq)
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		return err
	}
	for _, ev := range backlog {
		if write(ev) != nil {
			return
		}
	}
	if write(newEvent(harness.EventStreamReady, "", struct{}{})) != nil {
		return
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok || write(ev) != nil {
				return
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (c *convo) postMessage(w http.ResponseWriter, r *http.Request) {
	var in struct{ Text string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// a new instruction ends any approve_all
	c.grants = harness.Grants{}
	switch c.status {
	case harness.StatusRunning:
		// steering: the running run's Inbox delivers it before its next model call
		ev := newEvent(harness.EventUserQueued, "", userPayload{Text: in.Text})
		c.emitLocked(ev)
		c.queued = append(c.queued, queuedMsg{seq: int64(len(c.events)), text: in.Text})
	default:
		// the user moved on: the open confirmation or plan lapses
		if c.held != nil {
			c.history = append(c.history, harness.Message{Role: harness.RoleTool, ToolCallID: c.held.ID,
				Content: `{"status":"not_run","note":"the user's next message answers or replaces this call"}`})
			c.held = nil
		}
		c.deliverQueuedLocked()
		c.emitLocked(newEvent(harness.EventUserMessage, "", userPayload{Text: in.Text}))
		c.history = append(c.history, harness.Message{Role: harness.RoleUser, Content: in.Text})
		c.startLocked(nil, nil)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (c *convo) postDecision(w http.ResponseWriter, r *http.Request) {
	var d harness.DecisionPayload
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch d.Decision {
	case harness.DecisionApprove, harness.DecisionApproveAll, harness.DecisionReject:
	default:
		http.Error(w, "unknown decision", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// a late click on an old card, or a second tab deciding twice
	if c.held == nil || c.held.ID != d.ApprovalID {
		http.Error(w, harness.ErrStaleApproval.Error(), http.StatusConflict)
		return
	}
	// a real project checks here that the principal may approve
	d.ByID = "demo"
	ev := newEvent(harness.EventDecision, "", d)
	ev.ToolCallID = d.ApprovalID
	c.emitLocked(ev)
	var calls []string
	switch {
	case d.Decision == harness.DecisionReject:
	case c.held.Name == harness.ToolPlan:
		calls = harness.PlanCalls(*c.held)
	case d.Decision == harness.DecisionApproveAll:
		c.grants.Tools = append(c.grants.Tools, agent.GrantGroup(c.held.Name)...)
	}
	c.held = nil
	c.startLocked(&d, calls)
	w.WriteHeader(http.StatusAccepted)
}

// deliverQueuedLocked moves messages queued too late for the last run's Inbox into history.
func (c *convo) deliverQueuedLocked() {
	for _, q := range c.queued {
		c.emitLocked(newEvent(harness.EventUserMessage, "", userPayload{Text: q.text, QueuedSeq: q.seq}))
		c.history = append(c.history, harness.Message{Role: harness.RoleUser, Content: q.text})
	}
	c.queued = nil
}

// calls are an approved plan's steps; only the run the approval starts gets them.
func (c *convo) startLocked(d *harness.DecisionPayload, calls []string) {
	c.runs++
	runID := "r" + strconv.Itoa(c.runs)
	c.status = harness.StatusRunning
	c.emitLocked(newEvent(harness.EventStatus, runID, harness.StatusPayload{Status: c.status}))
	cfg := harness.RunConfig{
		RunID:    runID,
		History:  append([]harness.Message(nil), c.history...),
		Grants:   harness.Grants{Tools: append([]string(nil), c.grants.Tools...), Calls: calls},
		Decision: d,
	}
	go c.run(cfg)
}

func (c *convo) run(cfg harness.RunConfig) {
	runID := cfg.RunID
	live := func(typ harness.EventType, messageID, toolCallID string, payload any) {
		ev := newEvent(typ, runID, payload)
		ev.MessageID, ev.ToolCallID = messageID, toolCallID
		c.emit(ev)
	}
	cfg.Inbox = c.inbox
	cfg.OnDelta = func(id, text string) { live(harness.EventDelta, id, "", harness.DeltaPayload{Text: text}) }
	cfg.OnReasoning = func(id, text string) { live(harness.EventReasoning, id, "", harness.DeltaPayload{Text: text}) }
	cfg.OnReset = func(id string) { live(harness.EventStepReset, id, "", struct{}{}) }
	cfg.OnToolStart = func(id string, call harness.ToolCall, d harness.ToolDisplay) {
		live(harness.EventToolStart, id, call.ID, harness.ToolStartPayload{Name: call.Name, Display: d})
	}
	cfg.OnToolDone = func(call harness.ToolCall, out harness.ToolOutcome) {
		live(harness.EventToolDone, "", call.ID, harness.ToolDonePayload{OK: out.OK, Held: out.Held, DurationMs: out.Duration.Milliseconds()})
	}

	res, err := agent.Run(context.Background(), c.model, cfg)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistLocked(runID, res)
	c.history = append(c.history, res.Messages...)

	c.status = harness.StatusIdle
	switch {
	case err != nil:
		// the raw error only goes to the log
		log.Printf("run %s: %v", runID, err)
		c.emitLocked(newEvent(harness.EventError, runID, harness.ErrorPayload{Message: "Something went wrong. Try again."}))
		c.status = harness.StatusError
	case res.Confirm != nil:
		call := *res.Confirm
		c.held = &call
		ev := newEvent(harness.EventConfirm, runID, harness.ConfirmPayload{
			ApprovalID: call.ID,
			Call:       call,
			Tier:       agent.Tier(call).String(),
			Display:    agent.Display(call),
			Preview:    res.ConfirmPreview,
			GrantGroup: agent.GrantGroup(call.Name),
		})
		ev.ToolCallID = call.ID
		c.emitLocked(ev)
		c.status = harness.StatusAwaitingInput
	case res.Interaction != nil && res.Interaction.Name == harness.ToolPlan:
		call := *res.Interaction
		c.held = &call
		ev := newEvent(harness.EventPlan, runID, struct {
			Arguments json.RawMessage `json:"arguments"`
		}{call.Arguments})
		ev.ToolCallID = call.ID
		c.emitLocked(ev)
		c.status = harness.StatusAwaitingInput
	}
	c.emitLocked(newEvent(harness.EventStatus, runID, harness.StatusPayload{
		Status:     c.status,
		Stop:       res.Stop,
		Usage:      &res.Usage,
		Iterations: res.Iterations,
		Model:      res.Model,
	}))
	// a message queued after the run's last Inbox read still needs an answer
	if c.status == harness.StatusIdle && len(c.queued) > 0 {
		c.deliverQueuedLocked()
		c.startLocked(nil, nil)
	}
}

// inbox revokes the run's grants: any queued message is a new instruction.
func (c *convo) inbox(ctx context.Context) ([]harness.Message, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queued) == 0 {
		return nil, false, nil
	}
	msgs := make([]harness.Message, 0, len(c.queued))
	for _, q := range c.queued {
		msgs = append(msgs, harness.Message{Role: harness.RoleUser, Content: q.text})
		c.taken = append(c.taken, q.seq)
	}
	c.queued = nil
	return msgs, true, nil
}

// persistLocked: on reload, these tool_call/tool_result events replace the live
// tool_start/tool_done under the same ToolCallID.
func (c *convo) persistLocked(runID string, res *harness.RunResult) {
	for _, m := range res.Messages {
		switch {
		case m.Role == harness.RoleUser:
			// injected by inbox, in the order it read them
			c.emitLocked(newEvent(harness.EventUserMessage, runID, userPayload{Text: m.Content, QueuedSeq: c.taken[0]}))
			c.taken = c.taken[1:]
		case m.Role == harness.RoleTool:
			ev := newEvent(harness.EventToolResult, runID, struct {
				Content string `json:"content"`
			}{m.Content})
			ev.ToolCallID = m.ToolCallID
			c.emitLocked(ev)
		case m.Role == harness.RoleAssistant:
			if m.Content != "" {
				ev := newEvent(harness.EventMessage, runID, harness.MessagePayload{Text: m.Content, Model: res.Model})
				ev.MessageID = m.ID
				c.emitLocked(ev)
			}
			for _, call := range m.ToolCalls {
				ev := newEvent(harness.EventToolCall, runID, struct {
					harness.ToolCall
					Display harness.ToolDisplay `json:"display"`
				}{call, agent.Display(call)})
				ev.MessageID, ev.ToolCallID = m.ID, call.ID
				c.emitLocked(ev)
			}
		}
	}
}

func newModel(id, baseURL string, fake bool) harness.Model {
	if fake {
		return harnesstest.NewFakeModel(
			harnesstest.Call("c1", "now", struct{}{}),
			harnesstest.Call("c2", "restart_service", map[string]string{"service": "web"}),
			harnesstest.Text("Restarted web."),
		)
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" && baseURL == "" {
		log.Fatal("set OPENAI_API_KEY, or pass -base-url or -fake")
	}
	var opts []openaiharness.Option
	if baseURL != "" {
		opts = append(opts, openaiharness.WithBaseURL(baseURL))
	}
	return openaiharness.New(key, id, opts...)
}
