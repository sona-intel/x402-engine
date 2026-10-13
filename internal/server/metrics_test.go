package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"x402/internal/store"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestRefreshedExposesWatermarkLagAndDuration(t *testing.T) {
	m := NewMetrics()
	m.Refreshed("eip155:143", store.Meta{WatermarkBlock: 1000, IndexerProgressBlock: 1012, RefreshMS: 1500})
	body := scrape(t, m)
	for _, want := range []string{`x402_watermark_block{chain="eip155:143"} 1000`, `x402_lag_blocks{chain="eip155:143"} 12`, `x402_refresh_seconds_count{chain="eip155:143"} 1`, `x402_refresh_seconds_sum{chain="eip155:143"} 1.5`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestRefreshedLeavesTheGaugesAloneBeforeTheFirstIngest(t *testing.T) {
	m := NewMetrics()
	m.Refreshed("eip155:143", store.Meta{WatermarkBlock: -1, IndexerProgressBlock: 5, RefreshMS: 20})
	body := scrape(t, m)
	if !strings.Contains(body, `x402_refresh_seconds_count{chain="eip155:143"} 1`) {
		t.Error("metrics lack the refresh count")
	}
	for _, unwanted := range []string{"x402_watermark_block{", "x402_lag_blocks{"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("metrics hold %q before the first ingest", unwanted)
		}
	}
}

func TestPrunedCountsPerChain(t *testing.T) {
	m := NewMetrics()
	m.Pruned("eip155:8453", 40)
	m.Pruned("eip155:8453", 2)
	if body := scrape(t, m); !strings.Contains(body, `x402_pruned_payments_total{chain="eip155:8453"} 42`) {
		t.Errorf("metrics lack the pruned count\n%s", body)
	}
}

func TestPruneFailuresCountPerChain(t *testing.T) {
	m := NewMetrics()
	m.PruneFailed("eip155:8453")
	if body := scrape(t, m); !strings.Contains(body, `x402_prune_errors_total{chain="eip155:8453"} 1`) {
		t.Errorf("metrics lack the prune error count\n%s", body)
	}
}
