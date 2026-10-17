package harness

import (
	"context"
	"slices"
	"testing"
)

func TestValuesContains(t *testing.T) {
	match := Values{"tournamentId": "t1", "matchId": "m1"}
	cases := []struct {
		sub  Values
		want bool
	}{
		{nil, true},
		{Values{}, true},
		{Values{"tournamentId": "t1"}, true},
		{Values{"tournamentId": "t1", "matchId": "m1"}, true},
		{Values{"tournamentId": "t2"}, false},
		{Values{"serverId": "t1"}, false},
		{Values{"tournamentId": "t1", "matchId": "m2"}, false},
	}
	for _, c := range cases {
		if got := match.Contains(c.sub); got != c.want {
			t.Errorf("%v.Contains(%v) = %v, want %v", match, c.sub, got, c.want)
		}
	}
}

func TestWithValuesLayers(t *testing.T) {
	conv := Values{"tournamentId": "t1"}
	ctx := WithValues(context.Background(), conv)
	ctx = WithValues(ctx, Values{"page": "/t/x/bracket"})

	if got := Value(ctx, "tournamentId"); got != "t1" {
		t.Fatalf("tournamentId = %q", got)
	}
	if got := Value(ctx, "page"); got != "/t/x/bracket" {
		t.Fatalf("page = %q", got)
	}
	if _, ok := conv["page"]; ok {
		t.Fatal("WithValues modified its input")
	}
	if got := Value(context.Background(), "tournamentId"); got != "" {
		t.Fatalf("empty ctx = %q", got)
	}
}

func TestValuesMissing(t *testing.T) {
	v := Values{"serverId": "s1", "page": ""}
	if got := v.Missing([]string{"serverId", "page", "x"}); !slices.Equal(got, []string{"page", "x"}) {
		t.Fatalf("Missing = %v", got)
	}
}
