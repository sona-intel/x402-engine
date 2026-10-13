package problem

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewBuildsTheContractBody(t *testing.T) {
	want := map[string]int{
		"invalid_query":        http.StatusBadRequest,
		"invalid_id":           http.StatusBadRequest,
		"unsupported_chain":    http.StatusNotFound,
		"not_found":            http.StatusNotFound,
		"internal":             http.StatusInternalServerError,
		"upstream_unavailable": http.StatusServiceUnavailable,
		"deadline_exceeded":    http.StatusGatewayTimeout,
	}
	for code, status := range want {
		p := New(code, "d", "/x")
		if p.Type != "urn:sona:problem:"+code || p.Code != code || p.Status != status || p.Title == "" || p.Detail != "d" || p.Instance != "/x" {
			t.Errorf("%s: %+v", code, p)
		}
	}
}

func TestUpstreamUnavailableAsksForOneSecond(t *testing.T) {
	p := New("upstream_unavailable", "d", "/x")
	if p.Type != "urn:sona:problem:upstream_unavailable" || p.Status != 503 || p.RetryAfterS != 1 {
		t.Fatal(p)
	}
	if New("not_found", "", "").RetryAfterS != 0 {
		t.Fatal("retry hint on a 404")
	}
}

func TestUnknownCodeBecomesInternal(t *testing.T) {
	p := New("teapot", "d", "/x")
	if p.Code != "internal" || p.Status != 500 || p.Type != "urn:sona:problem:internal" {
		t.Fatal(p)
	}
}

func TestWriteSetsHeadersAndRoundTrips(t *testing.T) {
	for _, code := range []string{"upstream_unavailable", "not_found", "internal"} {
		want := New(code, "something is wrong", "/internal/v1/x")
		rec := httptest.NewRecorder()
		Write(rec, want)
		h := rec.Header()
		if rec.Code != want.Status || h.Get("Content-Type") != "application/problem+json" || h.Get("Cache-Control") != "no-store" {
			t.Fatal(code, rec.Code, h)
		}
		if retry := h.Get("Retry-After"); (retry == "1") != (code == "upstream_unavailable") || (retry != "" && retry != "1") {
			t.Errorf("%s: Retry-After %q", code, retry)
		}
		var got Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != want {
			t.Fatal(code, got, err)
		}
	}
}

func TestWriteOmitsEmptyDetailAndInstance(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New("not_found", "", ""))
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"detail", "instance", "retry_after_s"} {
		if _, ok := raw[key]; ok {
			t.Errorf("%s present: %v", key, raw)
		}
	}
}
