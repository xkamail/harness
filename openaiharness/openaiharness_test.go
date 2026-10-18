package openaiharness

import (
	"context"
	"errors"
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
