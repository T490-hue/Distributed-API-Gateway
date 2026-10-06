package logging

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type fakeLogger struct{ entries []Entry }

func (f *fakeLogger) Log(e Entry) { f.entries = append(f.entries, e) }

// The async logger must never block the request path, even when the buffer is
// full and nothing is draining it.
func TestAsyncLogNeverBlocks(t *testing.T) {
	a := NewAsync(nil, 2) // writer not started: buffer fills up
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			a.Log(Entry{Path: "/x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AsyncLogger.Log blocked on a full buffer")
	}
	if a.Dropped() != 98 { // buffer holds 2 of the 100 entries
		t.Fatalf("expected 98 dropped entries, got %d", a.Dropped())
	}
}

// Rejected requests must be logged with allowed=false, and the logged path must
// be the client-facing path even if a downstream handler rewrites it.
func TestMiddlewareLogsRejectionsAndOriginalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fl := &fakeLogger{}
	r := gin.New()
	r.Use(Middleware(fl))
	r.GET("/v1/proxy/*path", func(c *gin.Context) {
		c.Request.URL.Path = "/rewritten"
		c.AbortWithStatus(http.StatusTooManyRequests)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/proxy/get", nil))

	if len(fl.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(fl.entries))
	}
	e := fl.entries[0]
	if e.Allowed || e.StatusCode != 429 {
		t.Fatalf("expected rejected 429 entry, got %+v", e)
	}
	if e.Path != "/v1/proxy/get" {
		t.Fatalf("logged path = %q, want /v1/proxy/get", e.Path)
	}
}
