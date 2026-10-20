package server

import (
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Opt-in real-router/browser fixture. Ordinary tests never open this server.
// The existing fixture restores a stopped synthetic descriptor; no model is
// loaded. Its management key is a public test value, not a real credential.
func TestResourcesBrowserFixture(t *testing.T) {
	if os.Getenv("LLAMACTL_P3B_BROWSER_FIXTURE") != "1" {
		t.Skip("opt-in browser fixture; no model or server launch in ordinary tests")
	}
	router, _, _ := p3HTTPFixture(t)
	server := httptest.NewServer(router)
	defer server.Close()
	t.Logf("P3B isolated browser URL: %s ; synthetic management key: synthetic-management-key ; expires in 120 seconds", server.URL)
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
	}
}
