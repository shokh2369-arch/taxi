package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

func newHealthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:health_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestHealth_OK_AndCORSForFlutterLocalhost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHealthTestDB(t)
	r := gin.New()
	r.Use(corsMiddleware())
	r.GET("/health", healthHandler(db))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8101")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "OK" {
		t.Fatalf("body=%q", body)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8101" {
		t.Fatalf("Allow-Origin=%q", got)
	}
}

func TestHealth_ClientCancelDoesNotPoisonCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHealthTestDB(t)
	h := healthHandler(db)
	r := gin.New()
	r.GET("/health", h)

	// Cancelled request context must not make a later probe return 503.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx)
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("canceled-client probe status=%d body=%s (must still be OK)", w1.Code, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK || w2.Body.String() != "OK" {
		t.Fatalf("follow-up status=%d body=%s", w2.Code, w2.Body.String())
	}
}

func TestHealth_ConcurrentRequestsDoNotSerializeOnDB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHealthTestDB(t)
	r := gin.New()
	r.GET("/health", healthHandler(db))

	// Warm the cache so subsequent hits are lock-only.
	w0 := httptest.NewRecorder()
	r.ServeHTTP(w0, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w0.Code != http.StatusOK {
		t.Fatalf("warm status=%d", w0.Code)
	}

	const n = 20
	var wg sync.WaitGroup
	var failed atomic.Int32
	wg.Add(n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
			if w.Code != http.StatusOK {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d concurrent /health calls failed", failed.Load())
	}
	// Cached path should be near-instant even under concurrency.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("concurrent cached health took %s, want <1s", elapsed)
	}
}
