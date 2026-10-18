package harness

import (
	"context"
	"maps"
)

// Values is a conversation's server-set context, read by tools via Value; never
// model-supplied. pgharness keys storage, deletion and memory scope by containment.
type Values map[string]string

// Contains reports whether v holds every key of sub with the same value.
func (v Values) Contains(sub Values) bool {
	for k, s := range sub {
		if x, ok := v[k]; !ok || x != s {
			return false
		}
	}
	return true
}

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

// WithValues merges a copy of v over any Values already in ctx.
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

func Value(ctx context.Context, key string) string {
	return ValuesFrom(ctx)[key]
}
