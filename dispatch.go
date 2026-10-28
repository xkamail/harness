package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	defaultToolTimeout    = 30 * time.Second
	defaultMaxResultBytes = 16 << 10
	cacheMaxEntries       = 1024
	maxParallelCalls      = 8
)

const (
	resultSkipped  = "skipped: waiting for the user; call again after they reply"
	resultBlockAck = `{"ok":true,"shown":"the card is shown to the user"}`
	resultOK       = `{"ok":true}`
)

type registered struct {
	Tool
	schema *jsonschema.Schema
}

type cacheEntry struct {
	out     string
	expires time.Time
}

// resultCache is shared by every run of the agent.
type resultCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

func (c *resultCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !time.Now().Before(e.expires) {
		delete(c.entries, key)
		return "", false
	}
	return e.out, true
}

func (c *resultCache) put(key, out string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]cacheEntry)
	}
	now := time.Now()
	if len(c.entries) >= cacheMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= cacheMaxEntries {
			return // full of live entries: skip caching rather than grow
		}
	}
	c.entries[key] = cacheEntry{out: out, expires: now.Add(ttl)}
}

// callOut is JSON-encoded so a durable runner checkpoints the outcome with the result.
type callOut struct {
	Out string `json:"out"`
	OK  bool   `json:"ok"`
	// Checked: a gated call's Check passed; Preview is its result.
	Checked  bool            `json:"checked,omitempty"`
	Preview  json.RawMessage `json:"preview,omitempty"`
	Duration time.Duration   `json:"duration"`
	// SubCalls and SubUsage: what a sub-agent call's child did, checkpointed with the result.
	SubCalls []ToolCall `json:"subCalls,omitempty"`
	SubUsage Usage      `json:"subUsage,omitzero"`
}

func (o callOut) encode() string {
	b, _ := json.Marshal(o)
	return string(b)
}

func decodeOut(s string) callOut {
	var o callOut
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return callOut{Out: toolError("internal error")}
	}
	return o
}

type turn struct {
	// outs[i] answers calls[i]; a held call's is empty and not sent.
	outs        []string
	interaction *ToolCall
	confirm     *ToolCall
	preview     json.RawMessage
	blocks      []Block
	endsTurn    bool
	// subCalls[i] is calls[i]'s child tool calls; usage sums the children's.
	subCalls [][]ToolCall
	usage    Usage
}

// runTurn answers every call. At most one interaction or held confirmation
// per turn; later ones are skipped. force runs every call regardless of tier.
func (a *Agent) runTurn(ctx context.Context, messageID string, calls []ToolCall, granted *Grants, force bool, cfg RunConfig) (*turn, error) {
	c := a.c
	t := &turn{outs: make([]string, len(calls)), subCalls: make([][]ToolCall, len(calls))}
	for _, call := range calls {
		if cfg.OnToolStart != nil {
			cfg.OnToolStart(messageID, call, a.Display(call))
		}
	}
	done := func(i int, o callOut, held bool) {
		if cfg.OnToolDone != nil {
			cfg.OnToolDone(calls[i], ToolOutcome{OK: o.OK, Held: held, Duration: o.Duration})
		}
	}
	answer := func(i int, out string, ok bool) {
		t.outs[i] = out
		done(i, callOut{OK: ok}, false)
	}
	hold := func(i int, preview json.RawMessage) {
		t.confirm, t.preview = &calls[i], preview
		done(i, callOut{OK: true}, true)
	}

	var (
		idx      []int
		fns      []func(context.Context) (string, error)
		reported = make([]atomic.Bool, len(calls))
		isCheck  = make([]bool, len(calls))
		checked  []int // gated calls whose Check runs with the dispatches
	)
	rv := runValuesFrom(ctx)
	enqueue := func(i int, check bool) {
		call := calls[i]
		idx = append(idx, i)
		// a sub-agent call takes no slot: its child's tools need them
		isSub := !check && c.tools[call.Name] != nil && c.tools[call.Name].sub != nil
		fns = append(fns, func(ctx context.Context) (string, error) {
			// a durable runner hands fns a ctx not derived from Run's
			ctx = withRunValues(ctx, rv)
			var slot *subResult
			if isSub {
				slot = &subResult{}
				ctx = withSubSlot(ctx, slot)
			} else {
				rv.sem <- struct{}{}
				defer func() { <-rv.sem }()
			}
			start := time.Now()
			var (
				o   callOut
				err error
			)
			if check {
				o, err = c.check(ctx, call)
			} else {
				o, err = c.dispatch(ctx, call)
			}
			if err != nil {
				return "", err
			}
			o.Duration = time.Since(start)
			if slot != nil {
				o.SubCalls, o.SubUsage = slot.calls, slot.usage
			}
			// a check's outcome waits for the turn: it may be held
			if !check {
				reported[i].Store(true)
				done(i, o, false)
			}
			return o.encode(), nil
		})
	}

	for i, call := range calls {
		schema, isInteraction := c.interaction[call.Name]
		switch {
		case force:
			enqueue(i, false)
		case !isInteraction && a.Tier(call) == TierSafe:
			enqueue(i, false)
		case !isInteraction:
			switch {
			case t.interaction != nil || t.confirm != nil:
				answer(i, toolError(resultSkipped), false)
			case validateArgs(c.tools[call.Name].schema, call.Arguments) != nil:
				// let dispatch report bad args rather than confirm an unrunnable call
				enqueue(i, false)
			case a.granted(granted, call):
				enqueue(i, false)
			case c.tools[call.Name].Check != nil:
				checked, isCheck[i] = append(checked, i), true
				enqueue(i, true)
			default:
				hold(i, nil)
			}
		default:
			if err := validateArgs(schema, call.Arguments); err != nil {
				answer(i, toolError(err.Error()), false)
			} else if msg := a.planError(call); msg != "" {
				answer(i, toolError(msg), false)
			} else if t.interaction != nil {
				answer(i, toolError("skipped: "+t.interaction.Name+" already given this turn"), false)
			} else if t.confirm != nil {
				answer(i, toolError(resultSkipped), false)
			} else if msg := a.prepareProposal(ctx, &calls[i]); msg != "" {
				answer(i, toolError(msg), false)
			} else {
				t.interaction = &calls[i]
				answer(i, resultOK, true)
			}
		}
	}
	if len(fns) == 0 {
		return t, nil
	}
	run := cfg.Tools
	if run == nil {
		run = runConcurrently
	}
	res, err := run(ctx, fns)
	if err != nil {
		return nil, err
	}
	outs := make([]callOut, len(calls))
	for j, i := range idx {
		outs[i] = decodeOut(res[j])
		t.outs[i] = outs[i].Out
		t.subCalls[i] = outs[i].SubCalls
		t.usage = t.usage.Add(outs[i].SubUsage)
		// a durable runner replaying a turn returns results without running fns
		if !isCheck[i] && !reported[i].Load() {
			done(i, outs[i], false)
		}
	}
	for _, i := range checked {
		switch o := outs[i]; {
		case !o.OK:
			done(i, o, false) // the check failed; its error is the result
		case t.interaction != nil || t.confirm != nil:
			answer(i, toolError(resultSkipped), false)
		default:
			t.outs[i] = ""
			hold(i, o.Preview)
		}
	}
	ends, lookups := false, false
	for _, call := range calls {
		if tool, ok := c.tools[call.Name]; !ok || !tool.Block {
			lookups = true
		}
	}
	for _, i := range idx {
		tool, ok := c.tools[calls[i].Name]
		if !ok || !outs[i].OK || outs[i].Checked {
			continue
		}
		if tool.Block {
			t.blocks = append(t.blocks, Block{ToolCallID: calls[i].ID, Tool: tool.Name, Data: json.RawMessage(outs[i].Out)})
			t.outs[i] = resultBlockAck
		}
		ends = ends || tool.EndsTurn
	}
	// the model has not seen this round's non-card results yet
	t.endsTurn = ends && !lookups
	return t, nil
}

// granted uses up a matching plan step, which covers any tier; a tool grant never covers a destructive call.
func (a *Agent) granted(g *Grants, call ToolCall) bool {
	if i := slices.Index(g.Calls, CallKey(call)); i >= 0 {
		g.Calls = slices.Delete(g.Calls, i, i+1)
		return true
	}
	return slices.Contains(g.Tools, call.Name) && a.Tier(call) != TierDestructive
}

// planError reports a plan step naming an unknown tool; "" otherwise.
func (a *Agent) planError(call ToolCall) string {
	if call.Name != ToolPlan {
		return ""
	}
	var p struct {
		Steps []struct {
			Tool string `json:"tool"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return "invalid plan: " + err.Error()
	}
	for _, st := range p.Steps {
		if _, ok := a.c.tools[st.Tool]; !ok {
			return fmt.Sprintf("invalid plan: unknown tool %q", st.Tool)
		}
	}
	return ""
}

// prepareProposal writes completed args back into call, which is the
// assistant message's own, so history carries them; "" means accepted.
func (a *Agent) prepareProposal(ctx context.Context, call *ToolCall) string {
	if call.Name != ToolPropose || a.PrepareProposal == nil {
		return ""
	}
	args, err := a.PrepareProposal(ctx, call.Arguments)
	if err != nil {
		return err.Error()
	}
	if args != nil {
		call.Arguments = args
	}
	return ""
}

func runConcurrently(ctx context.Context, fns []func(context.Context) (string, error)) ([]string, error) {
	outs := make([]string, len(fns))
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Go(func() {
			outs[i], errs[i] = fn(ctx)
		})
	}
	wg.Wait()
	return outs, errors.Join(errs...)
}

// dispatch turns every failure into a model-visible {"error"} result; err is
// non-nil only when ctx is done.
func (c *compiled) dispatch(ctx context.Context, call ToolCall) (callOut, error) {
	out, err := c.dispatchTool(ctx, call)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return callOut{}, ctxErr
	}
	if err != nil {
		return callOut{Out: toolError(err.Error())}, nil
	}
	return callOut{Out: out, OK: true}, nil
}

func (c *compiled) dispatchTool(ctx context.Context, call ToolCall) (string, error) {
	t, ok := c.tools[call.Name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	if err := validateArgs(t.schema, call.Arguments); err != nil {
		return "", err
	}

	var key string
	if t.CacheTTL > 0 {
		key = CallKey(call)
		if out, ok := c.cache.get(key); ok {
			return out, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	res, err := callHandler(ctx, t.Name, t.Handler, call.Arguments)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("tool %q timed out", call.Name)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("tool %q result not serializable", call.Name)
	}
	out := string(b)
	// a card is not sent to the model, and a cut one is no longer JSON
	if !t.Block {
		out = capResult(out, t.MaxResultBytes)
	}
	if t.CacheTTL > 0 {
		c.cache.put(key, out, t.CacheTTL)
	}
	return out, nil
}

// check runs call's Check; err is non-nil only when ctx is done.
func (c *compiled) check(ctx context.Context, call ToolCall) (callOut, error) {
	t := c.tools[call.Name]
	cctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	preview, err := callHandler(cctx, t.Name, t.Check, call.Arguments)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return callOut{}, ctxErr
	}
	if err != nil {
		return callOut{Out: toolError(err.Error())}, nil
	}
	if preview == nil {
		return callOut{OK: true, Checked: true}, nil
	}
	b, err := json.Marshal(preview)
	if err != nil {
		return callOut{Out: toolError(fmt.Sprintf("tool %q preview not serializable", call.Name))}, nil
	}
	return callOut{OK: true, Checked: true, Preview: b}, nil
}

func callHandler(ctx context.Context, name string, h Handler, args json.RawMessage) (res any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool %q panicked: %v", name, p)
		}
	}()
	return h(ctx, args)
}

// PlanCalls returns the CallKeys of a plan call's steps, for Grants.Calls once it is approved.
func PlanCalls(plan ToolCall) []string {
	var p struct {
		Steps []struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"steps"`
	}
	if json.Unmarshal(plan.Arguments, &p) != nil {
		return nil
	}
	keys := make([]string, len(p.Steps))
	for i, st := range p.Steps {
		keys[i] = CallKey(ToolCall{Name: st.Tool, Arguments: st.Arguments})
	}
	return keys
}

// CallKey hashes name and key-order-independent args; approved plan steps match calls by it.
func CallKey(call ToolCall) string {
	args := bytes.TrimSpace(call.Arguments)
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber() // keep large integers distinct instead of rounding via float64
	if err := dec.Decode(&v); err == nil {
		if norm, err := json.Marshal(v); err == nil {
			args = norm
		}
	}
	sum := sha256.Sum256(args)
	return call.Name + ":" + hex.EncodeToString(sum[:])
}

// capResult truncates on a rune boundary and appends a marker for the model.
func capResult(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return fmt.Sprintf("%s…[truncated: result exceeded %d bytes]", s[:n], limit)
}

func compileSchema(name string, schema json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("harness: tool %q schema: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("tool.json", doc); err != nil {
		return nil, fmt.Errorf("harness: tool %q schema: %w", name, err)
	}
	s, err := c.Compile("tool.json")
	if err != nil {
		return nil, fmt.Errorf("harness: tool %q schema: %w", name, err)
	}
	return s, nil
}

func validateArgs(s *jsonschema.Schema, args json.RawMessage) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	if err := s.Validate(inst); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func toolError(msg string) string {
	b, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		return `{"error":"internal error"}`
	}
	return string(b)
}
