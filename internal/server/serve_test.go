package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewServerTimeouts(t *testing.T) {
	s := NewServer(Addr, http.NotFoundHandler())
	if s.Addr != Addr || s.ReadHeaderTimeout != 2*time.Second || s.ReadTimeout != 5*time.Second || s.WriteTimeout != 10*time.Second ||
		s.IdleTimeout != 120*time.Second || s.MaxHeaderBytes != 64<<10 {
		t.Errorf("server %+v", s)
	}
}

func TestDebugUsesTheServerTimeouts(t *testing.T) {
	d := Debug(DebugAddr, http.NotFoundHandler())
	if d.Addr != DebugAddr || d.ReadHeaderTimeout != 2*time.Second || d.ReadTimeout != 5*time.Second || d.WriteTimeout != 10*time.Second ||
		d.IdleTimeout != 120*time.Second || d.MaxHeaderBytes != 64<<10 {
		t.Errorf("debug server %+v", d)
	}
}

func TestDebugServesMetricsAndPprof(t *testing.T) {
	s := Debug(DebugAddr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("metrics")) }))
	for path, want := range map[string]int{"/metrics": 200, "/debug/pprof/": 200, "/debug/pprof/cmdline": 200, "/debug/pprof/heap": 200, "/other": 404} {
		rec := httptest.NewRecorder()
		s.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s -> %d", path, rec.Code)
		}
	}
}

func TestDebugProfileLongerThanWriteTimeoutStillAnswers200(t *testing.T) {
	srv := httptest.NewUnstartedServer(Debug(DebugAddr, http.NotFoundHandler()).Handler)
	srv.Config.WriteTimeout = 500 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/debug/pprof/profile?seconds=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatal(resp.StatusCode, len(body), err)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, NewServer("127.0.0.1:0", http.NotFoundHandler())) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunReturnsServeErrorAndStopsTheOthers(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), NewServer("127.0.0.1:0", http.NotFoundHandler()), NewServer("127.0.0.1:99999", http.NotFoundHandler()))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve error swallowed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestHealthyOnlyOnStatus200(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) }))
	t.Cleanup(srv.Close)
	if !healthy(srv.URL) {
		t.Fatal("200 reported unhealthy")
	}
	status.Store(http.StatusServiceUnavailable)
	if healthy(srv.URL) {
		t.Fatal("503 reported healthy")
	}
	srv.Close()
	if healthy(srv.URL) {
		t.Fatal("closed server reported healthy")
	}
}

func TestHealthcheckIgnoredWithoutSubcommand(t *testing.T) {
	HealthcheckIfRequested("/readyz")
}
