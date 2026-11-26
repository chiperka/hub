package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The tunnel from a cloud server ends on WorkerHandler: nothing but the worker
// protocol, even when the hub runs with authentication disabled.
func TestWorkerHandlerExposesOnlyTheWorkerProtocol(t *testing.T) {
	h := newHarness(t) // auth disabled, no join token: tokenless join is allowed on the main handler
	tun := httptest.NewServer(h.s.WorkerHandler())
	defer tun.Close()

	for _, path := range []string{"/api/cloud", "/api/runs", "/api/workers", "/api/tokens", "/"} {
		res, err := http.Get(tun.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s through the tunnel: status %d, want 404", path, res.StatusCode)
		}
	}

	res, err := http.Post(tun.URL+"/api/workers/register", "application/json", strings.NewReader(`{"name":"intruder"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("tokenless join through the tunnel: status %d, want 403", res.StatusCode)
	}
	if code := h.do("POST", "/api/workers/register", "", map[string]string{"name": "local"}, nil); code != http.StatusOK {
		t.Fatalf("tokenless join on the main listener (auth disabled) should still work, got %d", code)
	}
}
