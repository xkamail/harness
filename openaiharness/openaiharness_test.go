package openaiharness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xkamail/harness"
)

func TestStringErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"model is loading"}`))
	}))
	defer srv.Close()

	c := New("key", "m", WithBaseURL(srv.URL))
	_, err := c.Message(context.Background(), harness.Request{Messages: []harness.Message{{Role: harness.RoleUser, Content: "hi"}}})
	apiErr, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("err = %v, want *Error", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Message != "model is loading" || !apiErr.Retryable() {
		t.Fatalf("err = %+v", apiErr)
	}
}

func TestContextLength(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"413 any message", &Error{StatusCode: 413, Message: "payload too large"}, true},
		{"400 context_length", &Error{StatusCode: 400, Message: "context_length_exceeded"}, true},
		{"400 context length mixed case", &Error{StatusCode: 400, Message: "This model's maximum context length is 8192 tokens"}, true},
		{"400 maximum context", &Error{StatusCode: 400, Message: "Exceeds Maximum Context window"}, true},
		{"400 prompt too long", &Error{StatusCode: 400, Message: "Prompt is too long: 210000 tokens > 200000 maximum"}, true},
		{"400 input too long", &Error{StatusCode: 400, Message: "Input is too long for this model"}, true},
		{"400 unrelated", &Error{StatusCode: 400, Message: "invalid model"}, false},
		{"429 context length", &Error{StatusCode: 429, Message: "context length"}, false},
		{"500 context length", &Error{StatusCode: 500, Message: "context length"}, false},
		{"status 0", &Error{Message: "context length"}, false},
		{"wrapped", fmt.Errorf("call failed: %w", &Error{StatusCode: 413}), true},
		{"plain error", errors.New("context length"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := harness.IsContextLength(tt.err); got != tt.want {
				t.Fatalf("IsContextLength(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
