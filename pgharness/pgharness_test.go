package pgharness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/pgsql/pgctx"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rs/xid"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/harnesstest"
)

// setup returns a ctx on a fresh migrated database; TEST_DB_URL's %s is the database name.
func setup(t *testing.T) context.Context {
	t.Helper()
	tmpl := os.Getenv("TEST_DB_URL")
	if tmpl == "" || !strings.Contains(tmpl, "%s") {
		t.Fatalf("TEST_DB_URL must be set to a URL with a %%s database placeholder")
	}
	root, err := sql.Open("pgx", fmt.Sprintf(tmpl, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	name := "test_pgharness_" + xid.New().String()
	if _, err := root.Exec(`create database ` + name); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", fmt.Sprintf(tmpl, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		root.Exec(`drop database ` + name + ` with (force)`)
		root.Close()
	})
	ctx := pgctx.NewContext(context.Background(), db)
	if err := Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}

var testAgent = harness.MustCompile(harness.Agent{
	Name: "test",
	Tools: []harness.Tool{
		{Name: "now", Handler: func(context.Context, json.RawMessage) (any, error) { return "noon", nil }},
		{Name: "restart", Tier: harness.TierDisruptive, GrantGroup: "ops", Handler: func(context.Context, json.RawMessage) (any, error) { return "ok", nil }},
	},
})

func call(id, name string) harness.ToolCall {
	return harness.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func eventTypes(t *testing.T, ctx context.Context, id string, after int64) []harness.EventType {
	t.Helper()
	evs, err := Events(ctx, id, after, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]harness.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func mustStart(t *testing.T, ctx context.Context, bus Bus, p StartParams) (*Conversation, string) {
	t.Helper()
	if p.Agent == "" {
		p.Agent, p.OwnerID = "test", "u1"
	}
	c, runID, err := Start(ctx, bus, p)
	if err != nil {
		t.Fatal(err)
	}
	return c, runID
}

func mustLoad(t *testing.T, ctx context.Context, id, runID string) *Loaded {
	t.Helper()
	l, err := Load(ctx, id, runID)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustFinish(t *testing.T, ctx context.Context, id, runID string, res *harness.RunResult, runErr error) string {
	t.Helper()
	next, err := Finish(ctx, nil, testAgent, id, runID, res, runErr)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func roles(msgs []harness.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(string(m.Role)[:1])
	}
	return sb.String()
}

func TestMigrateTwice(t *testing.T) {
	ctx := setup(t)
	if err := Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStartLoadFinish(t *testing.T) {
	ctx := setup(t)
	c, run1 := mustStart(t, ctx, nil, StartParams{Message: "what time is it", Values: harness.Values{"page": "/home"}, Context: harness.Values{"serverId": "s1"}})
	if run1 == "" || c.Title != "what time is it" {
		t.Fatalf("run %q title %q", run1, c.Title)
	}

	l := mustLoad(t, ctx, c.ID, run1)
	if roles(l.History) != "u" || harness.Value(l.Ctx, "page") != "/home" || harness.Value(l.Ctx, "serverId") != "s1" {
		t.Fatalf("history %s ctx %v", roles(l.History), harness.ValuesFrom(l.Ctx))
	}
	if ConversationFrom(l.Ctx).ID != c.ID {
		t.Fatal("conversation missing from ctx")
	}

	res := &harness.RunResult{
		Model: "fake",
		Stop:  harness.StopReply,
		Usage: harness.Usage{InputTokens: 10, OutputTokens: 5},
		Messages: []harness.Message{
			{ID: run1 + ".1", Role: harness.RoleAssistant, Content: "checking", ToolCalls: []harness.ToolCall{call("c1", "now"), call("c2", "now")}},
			{Role: harness.RoleTool, ToolCallID: "c1", Content: `"noon"`, SubCalls: []harness.ToolCall{call("s1", "echo")}},
			{Role: harness.RoleTool, ToolCallID: "c2", Content: `"noon"`},
			{ID: run1 + ".2", Role: harness.RoleAssistant, Content: "It is noon."},
		},
		ContextTokens: 10,
	}
	if next := mustFinish(t, ctx, c.ID, run1, res, nil); next != "" {
		t.Fatalf("next run %q", next)
	}
	got := fmt.Sprint(eventTypes(t, ctx, c.ID, 0))
	want := "[user.message session.status agent.message agent.tool_call agent.tool_call agent.tool_result agent.tool_result agent.message session.status]"
	if got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	c, _ = Get(ctx, c.ID)
	if c.Status != harness.StatusIdle || c.ContextTokens != 10 {
		t.Fatalf("status %s tokens %d", c.Status, c.ContextTokens)
	}

	// a superseded run writes nothing
	if next := mustFinish(t, ctx, c.ID, run1, res, nil); next != "" || len(eventTypes(t, ctx, c.ID, 0)) != 9 {
		t.Fatal("stale Finish wrote")
	}
	if _, err := Load(ctx, c.ID, run1); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("Load stale run: %v", err)
	}

	run2, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "thanks"})
	if err != nil {
		t.Fatal(err)
	}
	l = mustLoad(t, ctx, c.ID, run2)
	if roles(l.History) != "uattau" || len(l.History[1].ToolCalls) != 2 || l.History[1].Content != "checking" {
		t.Fatalf("history %s %+v", roles(l.History), l.History[1])
	}
	if want := []harness.ToolCall{call("s1", "echo")}; !reflect.DeepEqual(l.History[2].SubCalls, want) || len(l.History[3].SubCalls) != 0 {
		t.Fatalf("subcalls %+v / %+v, want %+v / none", l.History[2].SubCalls, l.History[3].SubCalls, want)
	}
	if harness.Value(l.Ctx, "page") != "" {
		t.Fatal("per-message values leaked into the next message")
	}
}

func TestFinishError(t *testing.T) {
	ctx := setup(t)
	c, run := mustStart(t, ctx, nil, StartParams{Message: "hi"})
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{}, errors.New("boom"))
	evs, _ := Events(ctx, c.ID, 2, 10)
	var p harness.ErrorPayload
	json.Unmarshal(evs[0].Payload, &p)
	if evs[0].Type != harness.EventError || p.Message != msgError || strings.Contains(string(evs[0].Payload), "boom") {
		t.Fatalf("error event %s %s", evs[0].Type, evs[0].Payload)
	}
	c, _ = Get(ctx, c.ID)
	if c.Status != harness.StatusError {
		t.Fatalf("status %s", c.Status)
	}
	if _, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "again"}); err != nil {
		t.Fatal(err)
	}
}

func TestRetry(t *testing.T) {
	ctx := setup(t)
	c, run := mustStart(t, ctx, nil, StartParams{Message: "hi"})
	if again, err := Retry(ctx, nil, c.ID); err != nil || again != "" {
		t.Fatalf("retried a running conversation %q %v", again, err)
	}
	Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "queued"})
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{}, errors.New("boom"))

	retry, err := Retry(ctx, nil, c.ID)
	if err != nil || retry == "" || retry == run {
		t.Fatalf("retry %q %v", retry, err)
	}
	if got := fmt.Sprint(eventTypes(t, ctx, c.ID, 0)); got != "[user.message session.status user.queued agent.error session.status user.message session.status]" {
		t.Fatalf("events %s", got)
	}
	if l := mustLoad(t, ctx, c.ID, retry); roles(l.History) != "uu" {
		t.Fatalf("history %s", roles(l.History))
	}
	if again, err := Retry(ctx, nil, c.ID); err != nil || again != "" {
		t.Fatalf("retried a running conversation %q %v", again, err)
	}
	if _, err := Retry(ctx, nil, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing %v", err)
	}

	// a run that failed before running the approved call runs it on retry
	c = confirmed(t, ctx)
	if again, err := Retry(ctx, nil, c.ID); err != nil || again != "" {
		t.Fatalf("retried an awaiting conversation %q %v", again, err)
	}
	run, err = Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionApprove}})
	if err != nil {
		t.Fatal(err)
	}
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{}, errors.New("boom"))
	retry, err = Retry(ctx, nil, c.ID)
	if err != nil || retry == "" {
		t.Fatalf("retry %q %v", retry, err)
	}
	if l := mustLoad(t, ctx, c.ID, retry); l.Decision == nil || l.Decision.ApprovalID != "c1" {
		t.Fatalf("decision %+v", l.Decision)
	}
}

// confirmed returns a conversation awaiting a decision on call c1.
func confirmed(t *testing.T, ctx context.Context) *Conversation {
	t.Helper()
	c, run := mustStart(t, ctx, nil, StartParams{Message: "restart web"})
	held := call("c1", "restart")
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{
		Stop:     harness.StopConfirm,
		Confirm:  &held,
		Messages: []harness.Message{{ID: run + ".1", Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{held}}},
	}, nil)
	c, _ = Get(ctx, c.ID)
	if c.Status != harness.StatusAwaitingInput {
		t.Fatalf("status %s", c.Status)
	}
	return c
}

func TestDecision(t *testing.T) {
	ctx := setup(t)
	c := confirmed(t, ctx)

	_, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "other", Decision: harness.DecisionApprove}})
	if !errors.Is(err, harness.ErrStaleApproval) {
		t.Fatalf("wrong approval id: %v", err)
	}
	run, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionApprove, ByID: "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionApprove}})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("decision while running: %v", err)
	}

	l := mustLoad(t, ctx, c.ID, run)
	if l.Decision == nil || l.Decision.ApprovalID != "c1" || roles(l.History) != "ua" {
		t.Fatalf("decision %+v history %s", l.Decision, roles(l.History))
	}

	mustFinish(t, ctx, c.ID, run, &harness.RunResult{Stop: harness.StopReply, Messages: []harness.Message{
		{Role: harness.RoleTool, ToolCallID: "c1", Content: `"ok"`},
		{ID: run + ".1", Role: harness.RoleAssistant, Content: "Restarted."},
	}}, nil)
	// the decision is spent: a second one finds nothing open
	_, err = Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionApprove}})
	if !errors.Is(err, harness.ErrStaleApproval) {
		t.Fatalf("decision after it ran: %v", err)
	}
}

func TestLapsedApproval(t *testing.T) {
	ctx := setup(t)
	c := confirmed(t, ctx)
	run, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "never mind"})
	if err != nil {
		t.Fatal(err)
	}
	l := mustLoad(t, ctx, c.ID, run)
	if roles(l.History) != "uatu" || l.History[2].ToolCallID != "c1" || l.History[2].Content != supersededResult || l.Decision != nil {
		t.Fatalf("history %s %+v", roles(l.History), l.History)
	}
}

func TestHistoryBuilder(t *testing.T) {
	ev := func(typ harness.EventType, toolCallID string, payload any) harness.Event {
		return callEvent(typ, "", toolCallID, payload)
	}
	confirm := ev(harness.EventConfirm, "c1", harness.ConfirmPayload{ApprovalID: "c1", GrantGroup: []string{"restart", "scale"}})
	approveAll := ev(harness.EventDecision, "c1", harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionApproveAll})
	tc := ev(harness.EventToolCall, "c1", ToolCallPayload{Name: "restart"})

	var b historyBuilder
	for _, e := range []harness.Event{ev(harness.EventUserMessage, "", UserPayload{Text: "go"}), tc, confirm, approveAll} {
		if err := b.add(e); err != nil {
			t.Fatal(err)
		}
	}
	b.finish()
	if fmt.Sprint(b.grants) != "[restart scale]" || b.decision == nil {
		t.Fatalf("grants %v decision %v", b.grants, b.decision)
	}

	// a retried run whose decided call already has its result gets no decision
	b.add(ev(harness.EventToolResult, "c1", ToolResultPayload{Content: "ok"}))
	b.finish()
	if b.decision != nil {
		t.Fatal("decision kept after the call ran")
	}

	// a new user line ends approve_all
	b.add(ev(harness.EventUserMessage, "", UserPayload{Text: "next"}))
	if len(b.grants) != 0 {
		t.Fatalf("grants %v after a user message", b.grants)
	}

	b.add(ev(harness.EventSummary, "", SummaryPayload{Text: "earlier"}))
	if roles(b.msgs) != "s" {
		t.Fatalf("summary history %s", roles(b.msgs))
	}
}

func TestSummaryKeepsTail(t *testing.T) {
	var b historyBuilder
	evs := []harness.Event{
		{Seq: 1, Type: harness.EventUserMessage, Payload: json.RawMessage(`{"text":"one"}`)},
		{Seq: 2, Type: harness.EventMessage, Payload: json.RawMessage(`{"text":"a1"}`)},
		{Seq: 3, Type: harness.EventUserMessage, Payload: json.RawMessage(`{"text":"two"}`)},
		{Seq: 4, Type: harness.EventToolCall, ToolCallID: "c1", Payload: json.RawMessage(`{"name":"now"}`)},
		{Seq: 5, Type: harness.EventSummary, Payload: json.RawMessage(`{"text":"earlier","through":3}`)},
		{Seq: 6, Type: harness.EventUserMessage, Payload: json.RawMessage(`{"text":"three"}`)},
	}
	for _, ev := range evs {
		if err := b.add(ev); err != nil {
			t.Fatal(err)
		}
	}
	// the tail's open call still gets its synthetic result
	if roles(b.msgs) != "suatu" || b.msgs[1].Content != "two" || fmt.Sprint(b.seqs) != "[5 3 4 6 6]" {
		t.Fatalf("history %s %v", roles(b.msgs), b.seqs)
	}
}

func TestCompact(t *testing.T) {
	ctx := setup(t)
	a := harness.MustCompile(harness.Agent{Name: "test", Compaction: &harness.CompactionConfig{Threshold: 100, KeepTail: 1}})
	reply := func(id, run, text string, tokens int64) {
		t.Helper()
		mustFinish(t, ctx, id, run, &harness.RunResult{ContextTokens: tokens, Messages: []harness.Message{{ID: run + ".1", Role: harness.RoleAssistant, Content: text}}}, nil)
	}
	c, run := mustStart(t, ctx, nil, StartParams{Message: "one"})
	reply(c.ID, run, "a1", 50)
	if err := Compact(ctx, nil, a, harnesstest.NewFakeModel(), c.ID); err != nil {
		t.Fatalf("below threshold: %v", err)
	}

	run, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "two"})
	if err != nil {
		t.Fatal(err)
	}
	reply(c.ID, run, "a2", 200)
	m := harnesstest.NewFakeModel(harnesstest.Text("they said one"))
	if err := Compact(ctx, nil, a, m, c.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.Requests) != 1 || !strings.Contains(m.Requests[0].Messages[1].Content, "User: one") || strings.Contains(m.Requests[0].Messages[1].Content, "two") {
		t.Fatalf("summarized %+v", m.Requests)
	}
	// nothing new to cut: no model call
	if err := Compact(ctx, nil, a, harnesstest.NewFakeModel(), c.ID); err != nil {
		t.Fatalf("second compact: %v", err)
	}

	run, err = Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "three"})
	if err != nil {
		t.Fatal(err)
	}
	l := mustLoad(t, ctx, c.ID, run)
	if roles(l.History) != "suau" || !strings.Contains(l.History[0].Content, "they said one") || l.History[1].Content != "two" {
		t.Fatalf("history %s %+v", roles(l.History), l.History)
	}
}

func TestPlanGrants(t *testing.T) {
	ctx := setup(t)
	c, run := mustStart(t, ctx, nil, StartParams{Message: "fix it"})
	plan := harness.ToolCall{ID: "p1", Name: harness.ToolPlan, Arguments: json.RawMessage(`{"steps":[
		{"summary":"a","tool":"restart","arguments":{"svc":"a"}},
		{"summary":"b","tool":"restart","arguments":{"svc":"b"}}]}`)}
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{Stop: harness.StopInteraction, Interaction: &plan, Messages: []harness.Message{
		{ID: run + ".1", Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{plan}},
	}}, nil)
	run, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "p1", Decision: harness.DecisionApprove}})
	if err != nil {
		t.Fatal(err)
	}
	l := mustLoad(t, ctx, c.ID, run)
	if l.Decision == nil || fmt.Sprint(l.Grants.Calls) != fmt.Sprint(harness.PlanCalls(plan)) {
		t.Fatalf("decision %+v calls %v", l.Decision, l.Grants.Calls)
	}

	// the run calls step a, then stops on another tool's confirm: the next
	// run is granted only step b
	other := harness.ToolCall{ID: "c2", Name: "restart", Arguments: json.RawMessage(`{"svc":"c"}`)}
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{Stop: harness.StopConfirm, Confirm: &other, Messages: []harness.Message{
		{Role: harness.RoleTool, ToolCallID: "p1", Content: `{"status":"approved"}`},
		{ID: run + ".1", Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{{ID: "c1", Name: "restart", Arguments: json.RawMessage(`{"svc": "a"}`)}}},
		{Role: harness.RoleTool, ToolCallID: "c1", Content: `"ok"`},
		{ID: run + ".2", Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{other}},
	}}, nil)
	run, err = Send(ctx, nil, SendParams{ConversationID: c.ID, Decision: &harness.DecisionPayload{ApprovalID: "c2", Decision: harness.DecisionApprove}})
	if err != nil {
		t.Fatal(err)
	}
	l = mustLoad(t, ctx, c.ID, run)
	if want := harness.PlanCalls(plan)[1:]; fmt.Sprint(l.Grants.Calls) != fmt.Sprint(want) {
		t.Fatalf("calls %v, want %v", l.Grants.Calls, want)
	}

	// a new message ends the plan
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{Messages: []harness.Message{{Role: harness.RoleTool, ToolCallID: "c2", Content: `"ok"`}}}, nil)
	run, _ = Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "thanks"})
	if l := mustLoad(t, ctx, c.ID, run); len(l.Grants.Calls) != 0 {
		t.Fatalf("calls %v after a message", l.Grants.Calls)
	}
}

func TestSteering(t *testing.T) {
	ctx := setup(t)
	c, run := mustStart(t, ctx, nil, StartParams{Message: "deploy"})
	l := mustLoad(t, ctx, c.ID, run)

	if r, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "use staging"}); err != nil || r != "" {
		t.Fatalf("queue: %q %v", r, err)
	}
	msgs, revoke, err := l.Inbox(l.Ctx)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "use staging" || !revoke {
		t.Fatalf("inbox %v %v %v", msgs, revoke, err)
	}
	if msgs, _, _ := l.Inbox(l.Ctx); len(msgs) != 0 {
		t.Fatal("inbox delivered twice")
	}
	// queued after the run's last inbox read
	Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "and tag it"})

	next := mustFinish(t, ctx, c.ID, run, &harness.RunResult{Stop: harness.StopReply, Messages: []harness.Message{
		msgs[0],
		{ID: run + ".2", Role: harness.RoleAssistant, Content: "Deployed to staging."},
	}}, nil)
	if next == "" {
		t.Fatal("no follow-up run for the late message")
	}
	evs, _ := Events(ctx, c.ID, 0, 100)
	delivered := map[string]int64{}
	queued := map[int64]string{}
	for _, ev := range evs {
		var p UserPayload
		json.Unmarshal(ev.Payload, &p)
		switch ev.Type {
		case harness.EventUserQueued:
			queued[ev.Seq] = p.Text
		case harness.EventUserMessage:
			if p.QueuedSeq != 0 {
				delivered[p.Text] = p.QueuedSeq
			}
		}
	}
	if queued[delivered["use staging"]] != "use staging" || queued[delivered["and tag it"]] != "and tag it" {
		t.Fatalf("queued %v delivered %v", queued, delivered)
	}
	l = mustLoad(t, ctx, c.ID, next)
	if roles(l.History) != "uuau" || l.History[3].Content != "and tag it" {
		t.Fatalf("history %s", roles(l.History))
	}
}

func TestQueueFull(t *testing.T) {
	ctx := setup(t)
	c, _ := mustStart(t, ctx, nil, StartParams{Message: "go"})
	for i := range maxQueued {
		if _, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "one more"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("got %v", err)
	}
}

// queuedSeqs maps each user.queued text to its seq.
func queuedSeqs(t *testing.T, ctx context.Context, id string) map[string]int64 {
	t.Helper()
	evs, err := Events(ctx, id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, ev := range evs {
		if ev.Type != harness.EventUserQueued {
			continue
		}
		var p UserPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.Text] = ev.Seq
	}
	return out
}

func TestUnqueue(t *testing.T) {
	ctx := setup(t)
	c, _ := mustStart(t, ctx, nil, StartParams{Message: "go"})
	for _, m := range []string{"a", "b"} {
		if _, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	q := queuedSeqs(t, ctx, c.ID)
	if err := Unqueue(ctx, nil, c.ID, q["a"]); err != nil {
		t.Fatal(err)
	}
	evs, _ := Events(ctx, c.ID, 0, 100)
	var unqueued []int64
	for _, ev := range evs {
		if ev.Type == harness.EventUserUnqueued {
			var p UnqueuedPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			unqueued = append(unqueued, p.QueuedSeq)
		}
	}
	if fmt.Sprint(unqueued) != fmt.Sprint([]int64{q["a"]}) {
		t.Fatalf("unqueued %v, want [%d]", unqueued, q["a"])
	}
	if err := Unqueue(ctx, nil, c.ID, q["a"]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("twice: %v", err)
	}
	if err := Unqueue(ctx, nil, c.ID, 1); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("user.message seq: %v", err)
	}
	if err := Unqueue(ctx, nil, "nope", q["a"]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown conversation: %v", err)
	}

	// delivery skips it
	if _, err := Stop(ctx, nil, c.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ = Events(ctx, c.ID, 0, 100)
	delivered := map[string]int64{}
	for _, ev := range evs {
		var p UserPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if ev.Type == harness.EventUserMessage && p.QueuedSeq != 0 {
			delivered[p.Text] = p.QueuedSeq
		}
	}
	if len(delivered) != 1 || delivered["b"] != q["b"] {
		t.Fatalf("delivered %v, want only b=%d", delivered, q["b"])
	}
	if err := Unqueue(ctx, nil, c.ID, q["b"]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("delivered: %v", err)
	}

	// the inbox skips it, and pairing is by ID even when a message is unqueued after the inbox read
	c2, run := mustStart(t, ctx, nil, StartParams{Message: "go"})
	l := mustLoad(t, ctx, c2.ID, run)
	for _, m := range []string{"x", "y"} {
		if _, err := Send(ctx, nil, SendParams{ConversationID: c2.ID, Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	q2 := queuedSeqs(t, ctx, c2.ID)
	if err := Unqueue(ctx, nil, c2.ID, q2["x"]); err != nil {
		t.Fatal(err)
	}
	msgs, _, err := l.Inbox(l.Ctx)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "y" || msgs[0].ID != strconv.FormatInt(q2["y"], 10) {
		t.Fatalf("inbox %+v %v", msgs, err)
	}
	if _, err := Send(ctx, nil, SendParams{ConversationID: c2.ID, Message: "z"}); err != nil {
		t.Fatal(err)
	}
	more, _, err := l.Inbox(l.Ctx)
	q2 = queuedSeqs(t, ctx, c2.ID)
	if err != nil || len(more) != 1 || more[0].Content != "z" || more[0].ID != strconv.FormatInt(q2["z"], 10) {
		t.Fatalf("inbox %+v %v", more, err)
	}
	if err := Unqueue(ctx, nil, c2.ID, q2["z"]); err != nil {
		t.Fatalf("unqueue after inbox read: %v", err)
	}
	res := &harness.RunResult{Stop: harness.StopReply, Messages: append(append(msgs, more...),
		harness.Message{ID: run + ".1", Role: harness.RoleAssistant, Content: "ok"})}
	if next := mustFinish(t, ctx, c2.ID, run, res, nil); next != "" {
		t.Fatalf("unqueued messages started run %q", next)
	}
	evs, _ = Events(ctx, c2.ID, 0, 100)
	delivered = map[string]int64{}
	for _, ev := range evs {
		var p UserPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if ev.Type == harness.EventUserMessage && p.QueuedSeq != 0 {
			delivered[p.Text] = p.QueuedSeq
		}
	}
	if len(delivered) != 2 || delivered["y"] != q2["y"] || delivered["z"] != q2["z"] {
		t.Fatalf("delivered %v, want y=%d z=%d", delivered, q2["y"], q2["z"])
	}
}

func TestStopAndOrphans(t *testing.T) {
	ctx := setup(t)
	c, run := mustStart(t, ctx, nil, StartParams{Message: "go"})
	Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "queued"})
	stopped, err := Stop(ctx, nil, c.ID)
	if err != nil || stopped != run {
		t.Fatalf("stop %q %v", stopped, err)
	}
	if next := mustFinish(t, ctx, c.ID, run, &harness.RunResult{Messages: []harness.Message{{Role: harness.RoleAssistant, Content: "late"}}}, nil); next != "" {
		t.Fatal("stopped run started another")
	}
	if got := fmt.Sprint(eventTypes(t, ctx, c.ID, 0)); got != "[user.message session.status user.queued user.message session.status]" {
		t.Fatalf("events %s", got)
	}
	if again, _ := Stop(ctx, nil, c.ID); again != "" {
		t.Fatal("stopped an idle conversation")
	}

	c2, _ := mustStart(t, ctx, nil, StartParams{Message: "go"})
	if err := FailOrphanedRuns(ctx, nil, 0, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	c2, _ = Get(ctx, c2.ID)
	if c2.Status != harness.StatusError {
		t.Fatalf("orphan status %s", c2.Status)
	}
}

func TestPolicies(t *testing.T) {
	ctx := setup(t)
	v := harness.Values{"serverId": "s1"}
	a, run := mustStart(t, ctx, nil, StartParams{Agent: "test", OwnerID: "u1", Context: v, Policy: PolicyPerContext, Message: "one"})
	b, queued := mustStart(t, ctx, nil, StartParams{Agent: "test", OwnerID: "u1", Context: v, Policy: PolicyPerContext, Message: "two"})
	if a.ID != b.ID || run == "" || queued != "" {
		t.Fatalf("per-context: %s %s run %q queued %q", a.ID, b.ID, run, queued)
	}
	other, _ := mustStart(t, ctx, nil, StartParams{Agent: "test", OwnerID: "u2", Context: v, Policy: PolicyPerContext, Message: "x"})
	shared, _ := mustStart(t, ctx, nil, StartParams{Agent: "test", OwnerID: "u3", Context: v, Policy: PolicyShared, Message: "x"})
	fresh, _ := mustStart(t, ctx, nil, StartParams{Agent: "test", OwnerID: "u1", Context: v, Policy: PolicyNew, Message: "x"})
	if other.ID == a.ID || fresh.ID == a.ID || shared.ID == "" {
		t.Fatal("policies resumed the wrong conversation")
	}
	if _, _, err := Start(ctx, nil, StartParams{Agent: "test", OwnerID: "u1", Require: []string{"serverId"}, Message: "x"}); !errors.Is(err, ErrContext) {
		t.Fatalf("missing required key: %v", err)
	}
}

func TestSetTitle(t *testing.T) {
	ctx := setup(t)
	c, _ := mustStart(t, ctx, nil, StartParams{Message: "hello"})
	if err := SetTitle(ctx, c.ID, "  Server  restart\nplan "); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, ctx, c.ID); got.Title != "Server restart plan" || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("title %q updated %v -> %v", got.Title, c.UpdatedAt, got.UpdatedAt)
	}
	if err := SetTitle(ctx, c.ID, strings.Repeat("x", 100)); err != nil {
		t.Fatal(err)
	}
	if got := []rune(mustGet(t, ctx, c.ID).Title); len(got) != 80 || got[79] != '…' {
		t.Fatalf("long title: %d runes", len(got))
	}
	if err := SetTitle(ctx, xid.New().String(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := setup(t)
	v := harness.Values{"serverId": "s1"}
	p := StartParams{Agent: "test", OwnerID: "u1", Context: v, Policy: PolicyPerContext, Message: "hi"}
	c, run := mustStart(t, ctx, nil, p)
	if err := Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if next := mustFinish(t, ctx, c.ID, run, &harness.RunResult{}, nil); next != "" {
		t.Fatalf("finish after delete started %q", next)
	}
	if _, err := Get(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if _, err := Load(ctx, c.ID, run); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load: %v", err)
	}
	if err := SetTitle(ctx, c.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set title: %v", err)
	}
	if _, err := Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("send: %v", err)
	}
	if _, err := Retry(ctx, nil, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry: %v", err)
	}
	if err := Delete(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := Delete(ctx, xid.New().String()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := FailOrphanedRuns(ctx, nil, 0, func(string) bool { return false }); err != nil {
		t.Fatalf("orphan sweep: %v", err)
	}
	if next, _ := mustStart(t, ctx, nil, p); next.ID == c.ID {
		t.Fatal("start resumed a deleted conversation")
	}
}

func TestList(t *testing.T) {
	ctx := setup(t)
	a, _ := mustStart(t, ctx, nil, StartParams{Message: "a"})
	b, _ := mustStart(t, ctx, nil, StartParams{Message: "b"})
	d, _ := mustStart(t, ctx, nil, StartParams{Message: "d"})
	c, _ := mustStart(t, ctx, nil, StartParams{Message: "c"})
	mustStart(t, ctx, nil, StartParams{Agent: "other", OwnerID: "u1", Message: "x"})
	mustStart(t, ctx, nil, StartParams{Agent: "test", OwnerID: "u2", Message: "x"})
	if err := Delete(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	ids := func(limit int) []string {
		got, err := List(ctx, "u1", "test", limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range got {
			out = append(out, x.ID)
		}
		return out
	}
	if got := ids(10); !slices.Equal(got, []string{c.ID, b.ID, a.ID}) {
		t.Fatalf("list %v, want %v", got, []string{c.ID, b.ID, a.ID})
	}
	if got := ids(2); !slices.Equal(got, []string{c.ID, b.ID}) {
		t.Fatalf("limit 2: %v", got)
	}
	got, err := List(ctx, "nobody", "test", 10)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")

func TestAttachments(t *testing.T) {
	ctx := setup(t)
	c, run := mustStart(t, ctx, nil, StartParams{Message: "look", Parts: []harness.Part{{Type: harness.PartImage, Name: "a.png", Data: png}}})
	evs, _ := Events(ctx, c.ID, 0, 1)
	if bytes.Contains(evs[0].Payload, []byte(`"data"`)) {
		t.Fatal("bytes stored in the event")
	}
	l := mustLoad(t, ctx, c.ID, run)
	p := l.History[0].Parts[0]
	if p.AttachmentID == "" || !bytes.Equal(p.Data, png) || p.MIME != "image/png" {
		t.Fatalf("part %+v", p)
	}

	// over budget: the newest message loads, older ones are omitted
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{Messages: []harness.Message{{Role: harness.RoleAssistant, Content: "ok"}}}, nil)
	Send(ctx, nil, SendParams{ConversationID: c.ID, Message: "again", Parts: []harness.Part{{Type: harness.PartImage, Data: png}}})
	l, _ = Load(ctx, c.ID, mustGet(t, ctx, c.ID).RunID)
	if err := attachParts(ctx, c.ID, l.History, 0); err != nil {
		t.Fatal(err)
	}
	if l.History[0].Parts[0].Type != harness.PartText || l.History[2].Parts[0].Data == nil {
		t.Fatalf("budget: %+v / %+v", l.History[0].Parts[0], l.History[2].Parts[0])
	}
}

func mustGet(t *testing.T, ctx context.Context, id string) *Conversation {
	t.Helper()
	c, err := Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type mapBlobs map[string][]byte

func (m mapBlobs) Put(_ context.Context, key, _ string, data []byte) error { m[key] = data; return nil }
func (m mapBlobs) Get(_ context.Context, key string) ([]byte, error) {
	b, ok := m[key]
	if !ok {
		return nil, ErrBlobNotFound
	}
	return b, nil
}
func (m mapBlobs) DeletePrefix(_ context.Context, prefix string) error {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
	return nil
}

func TestBlobsAndDeleteContext(t *testing.T) {
	blobs := mapBlobs{}
	ctx := WithBlobs(setup(t), blobs)
	gone, run := mustStart(t, ctx, nil, StartParams{Context: harness.Values{"serverId": "a", "matchId": "m"}, Message: "x", Parts: []harness.Part{{Type: harness.PartImage, Data: png}}})
	kept, _ := mustStart(t, ctx, nil, StartParams{Context: harness.Values{"serverId": "b"}, Message: "x"})
	if len(blobs) != 1 {
		t.Fatalf("blobs %d", len(blobs))
	}
	if l := mustLoad(t, ctx, gone.ID, run); !bytes.Equal(l.History[0].Parts[0].Data, png) {
		t.Fatal("blob not loaded")
	}
	clear(blobs)
	if l := mustLoad(t, ctx, gone.ID, run); !bytes.Equal(l.History[0].Parts[0].Data, png) {
		t.Fatal("blob not served from the cache")
	}
	blobCache = newByteLRU(64 << 20)
	if l := mustLoad(t, ctx, gone.ID, run); l.History[0].Parts[0].Type != harness.PartText {
		t.Fatal("missing blob not omitted")
	}

	_, err := pgctx.Exec(ctx, `
		insert into harness.memories (id, owner_id, kind, content, context)
		values ('m1', 'u1', 'fact', 'scoped', '{"serverId":"a"}'), ('m2', 'u1', 'fact', 'owner-wide', '{}')
	`)
	if err != nil {
		t.Fatal(err)
	}
	blobs["harness/"+gone.ID+"/x"] = png
	if err := DeleteContext(ctx, harness.Values{"serverId": "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("conversation not deleted")
	}
	if _, err := Get(ctx, kept.ID); err != nil {
		t.Fatal("other conversation deleted")
	}
	var ids string
	pgctx.QueryRow(ctx, `select string_agg(id, ',') from harness.memories`).Scan(&ids)
	if ids != "m2" || len(blobs) != 0 {
		t.Fatalf("memories %q blobs %d", ids, len(blobs))
	}
}

func TestMemory(t *testing.T) {
	ctx := setup(t)
	a := harness.MustCompile(WithMemory(harness.Agent{Name: "mem"}, MemoryOptions{Max: 2}))
	c, run := mustStart(t, ctx, nil, StartParams{Context: harness.Values{"serverId": "s1"}, Message: "hi"})
	l := mustLoad(t, ctx, c.ID, run)
	tool := func(name string) harness.Handler {
		for _, tl := range a.Tools {
			if tl.Name == name {
				return tl.Handler
			}
		}
		t.Fatalf("no tool %s", name)
		return nil
	}

	out, err := tool(ToolRemember)(l.Ctx, json.RawMessage(`{"kind":"preference","content":"likes short answers","scope":"owner"}`))
	if err != nil {
		t.Fatal(err)
	}
	saved := out.(Memory)
	if _, err := tool(ToolRemember)(l.Ctx, json.RawMessage(`{"kind":"fact","content":"runs paper","scope":"conversation"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool(ToolRemember)(l.Ctx, json.RawMessage(`{"kind":"fact","content":"third","scope":"owner"}`)); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("over MaxStored: %v", err)
	}
	text, err := a.Context(l.Ctx)
	if err != nil || !strings.Contains(text, "likes short answers") || !strings.Contains(text, "runs paper") {
		t.Fatalf("context %q %v", text, err)
	}

	// another subject sees only the owner-wide memory
	c2, run2 := mustStart(t, ctx, nil, StartParams{Context: harness.Values{"serverId": "s2"}, Message: "hi"})
	text, _ = a.Context(mustLoad(t, ctx, c2.ID, run2).Ctx)
	if strings.Contains(text, "runs paper") || !strings.Contains(text, "likes short answers") {
		t.Fatalf("other subject sees %q", text)
	}

	// results become agent.memory events
	b, _ := json.Marshal(saved)
	mustFinish(t, ctx, c.ID, run, &harness.RunResult{Messages: []harness.Message{
		{ID: run + ".1", Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{call("m", ToolRemember)}},
		{Role: harness.RoleTool, ToolCallID: "m", Content: string(b)},
		{ID: run + ".2", Role: harness.RoleAssistant, Content: "Noted."},
	}}, nil)
	if got := fmt.Sprint(eventTypes(t, ctx, c.ID, 2)); !strings.Contains(got, "agent.tool_result agent.memory") {
		t.Fatalf("events %s", got)
	}

	if _, err := tool(ToolForget)(l.Ctx, json.RawMessage(`{"id":"`+saved.ID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool(ToolForget)(l.Ctx, json.RawMessage(`{"id":"`+saved.ID+`"}`)); err == nil {
		t.Fatal("forgot twice")
	}
}

func TestFollow(t *testing.T) {
	ctx := setup(t)
	bus := NewMemoryBus()
	c, run := mustStart(t, ctx, bus, StartParams{Message: "hi"})

	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan harness.Event, 100)
	done := make(chan error, 1)
	go func() {
		done <- Follow(fctx, bus, c.ID, 1, FollowConfig{Heal: time.Hour}, func(ev harness.Event) error {
			got <- ev
			return nil
		})
	}()
	next := func() harness.Event {
		select {
		case ev := <-got:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return harness.Event{}
		}
	}
	if ev := next(); ev.Seq != 2 {
		t.Fatalf("catch-up from seq 2, got %d %s", ev.Seq, ev.Type)
	}
	if ev := next(); ev.Type != harness.EventStreamReady {
		t.Fatalf("got %s", ev.Type)
	}

	Live(ctx, bus, c.ID, run, harness.EventDelta, run+".1", "", harness.DeltaPayload{Text: "he"})
	if ev := next(); ev.Type != harness.EventDelta || ev.Seq != 0 {
		t.Fatalf("got %s", ev.Type)
	}
	Send(ctx, bus, SendParams{ConversationID: c.ID, Message: "queued"})
	if ev := next(); ev.Type != harness.EventUserQueued || ev.Seq != 3 {
		t.Fatalf("got %d %s", ev.Seq, ev.Type)
	}

	// nil bus simulates a lost publish: the follower catches up, in order and once,
	// when the next persisted event signals it.
	_, err := Finish(ctx, nil, testAgent, c.ID, run, &harness.RunResult{Messages: []harness.Message{{ID: run + ".1", Role: harness.RoleAssistant, Content: "hello"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	Live(ctx, bus, c.ID, run, harness.EventToolDone, "", "c1", harness.ToolDonePayload{OK: true})
	var seqs []int64
	for {
		ev := next()
		if ev.Seq == 0 {
			if ev.Type != harness.EventToolDone {
				t.Fatalf("got %s", ev.Type)
			}
			break
		}
		seqs = append(seqs, ev.Seq)
	}
	if len(seqs) != 0 {
		t.Fatalf("caught up before a signal: %v", seqs)
	}
	Send(ctx, bus, SendParams{ConversationID: c.ID, Message: "again"})
	want := int64(4)
	for want <= 8 {
		ev := next()
		if ev.Seq != want {
			t.Fatalf("seq %d, want %d", ev.Seq, want)
		}
		want++
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("follow ended with %v", err)
	}
}

func TestRecall(t *testing.T) {
	ctx := setup(t)
	if a := WithMemory(harness.Agent{Name: "mem"}, MemoryOptions{Max: 2}); len(a.Tools) != 2 {
		t.Fatalf("recall added while every memory fits: %d tools", len(a.Tools))
	}
	a := WithMemory(harness.Agent{Name: "mem"}, MemoryOptions{Max: 1, MaxStored: 10})
	recall := a.Tools[2].Handler
	c, run := mustStart(t, ctx, nil, StartParams{Context: harness.Values{"serverId": "s1"}, Message: "hi"})
	_, err := pgctx.Exec(ctx, `
		insert into harness.memories (id, owner_id, kind, content, context)
		values
			('m1', 'u1', 'preference', 'deploys to staging before production', '{}'),
			('m2', 'u1', 'fact', 'ผู้ใช้ชอบคำตอบสั้นๆ', '{}'),
			('m3', 'u1', 'fact', 'staging on another server', '{"serverId":"s2"}'),
			('m4', 'u2', 'fact', 'someone else uses staging', '{}')
	`)
	if err != nil {
		t.Fatal(err)
	}
	l := mustLoad(t, ctx, c.ID, run)
	ids := func(q string) string {
		t.Helper()
		out, err := recall(l.Ctx, json.RawMessage(`{"query":`+strconv.Quote(q)+`}`))
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, m := range out.([]Memory) {
			s = append(s, m.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids("staging"); got != "m1" {
		t.Fatalf("staging: %q", got)
	}
	if got := ids("คำตอบสั้น"); got != "m2" {
		t.Fatalf("thai: %q", got)
	}
	if got := ids("kubernetes"); got != "" {
		t.Fatalf("unrelated: %q", got)
	}
}
