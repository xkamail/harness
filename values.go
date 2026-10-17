package harness

import (
	"context"
	"maps"
)

// Values is a conversation's context: flat string keys and values the
// project sets when the conversation starts, such as
// {"tournamentId": "…", "matchId": "…"}. The harness puts them in every
// run's ctx, where tools read them with Value; they come from the server,
// never from the model, so no prompt can point a tool elsewhere.
//
// Values also key storage (pgharness): a conversation is found by its exact
// Values, everything carrying a set of Values is deleted with its
// containment (so deleting {"tournamentId": x} takes its matches too), and
// a memory is visible where the conversation's Values contain the memory's.
//
// Keep values flat strings, set them once at start, and wrap each key in a
// typed accessor so a misspelt key is a compile error:
//
//	func MatchID(ctx context.Context) string { return harness.Value(ctx, "matchId") }
type Values map[string]string

// Contains reports whether v holds every key of sub with the same value.
// Every Values contains an empty one.
func (v Values) Contains(sub Values) bool {
	for k, s := range sub {
		if x, ok := v[k]; !ok || x != s {
			return false
		}
	}
	return true
}

// Missing returns the keys of required that v lacks or holds empty.
func (v Values) Missing(required []string) []string {
	var out []string
	for _, k := range required {
		if v[k] == "" {
			out = append(out, k)
		}
	}
	return out
}

type valuesKey struct{}

// WithValues returns ctx carrying v merged over any Values already in ctx,
// so a run's per-message values (such as the page the user is on) layer on
// the conversation's. v is copied.
func WithValues(ctx context.Context, v Values) context.Context {
	merged := maps.Clone(ValuesFrom(ctx))
	if merged == nil {
		merged = make(Values, len(v))
	}
	maps.Copy(merged, v)
	return context.WithValue(ctx, valuesKey{}, merged)
}

// ValuesFrom returns the Values in ctx; do not modify the result.
func ValuesFrom(ctx context.Context) Values {
	v, _ := ctx.Value(valuesKey{}).(Values)
	return v
}

// Value returns the value of key in ctx, or "".
func Value(ctx context.Context, key string) string {
	return ValuesFrom(ctx)[key]
}
