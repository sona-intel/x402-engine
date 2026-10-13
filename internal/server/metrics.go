package server

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"x402/internal/store"
)

type Metrics struct {
	Registry  *prometheus.Registry
	requests  *prometheus.HistogramVec
	refresh   *prometheus.HistogramVec
	watermark *prometheus.GaugeVec
	lag       *prometheus.GaugeVec
	pruned    *prometheus.CounterVec
	pruneErrs *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		requests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "x402_request_duration_seconds",
			Help:    "Duration of served requests",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "code"}),
		refresh: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "x402_refresh_seconds",
			Help:    "Duration of refresh rounds that found the indexer initialised",
			Buckets: []float64{.01, .05, .25, 1, 5, 30, 120, 600, 3600},
		}, []string{"chain"}),
		watermark: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "x402_watermark_block", Help: "Last block copied from the indexer"}, []string{"chain"}),
		lag:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "x402_lag_blocks", Help: "Indexer progress block minus the watermark block"}, []string{"chain"}),
		pruned:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "x402_pruned_payments_total", Help: "Payment rows deleted past the retention"}, []string{"chain"}),
		pruneErrs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "x402_prune_errors_total", Help: "Prune runs that failed"}, []string{"chain"}),
	}
	m.Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), m.requests,
		m.refresh, m.watermark, m.lag, m.pruned, m.pruneErrs)
	return m
}

func (m *Metrics) Refreshed(chain string, meta store.Meta) {
	m.refresh.WithLabelValues(chain).Observe(float64(meta.RefreshMS) / 1000)
	if meta.WatermarkBlock >= 0 {
		m.watermark.WithLabelValues(chain).Set(float64(meta.WatermarkBlock))
		m.lag.WithLabelValues(chain).Set(float64(meta.IndexerProgressBlock - meta.WatermarkBlock))
	}
}

func (m *Metrics) Pruned(chain string, payments int64) {
	m.pruned.WithLabelValues(chain).Add(float64(payments))
}

func (m *Metrics) PruneFailed(chain string) { m.pruneErrs.WithLabelValues(chain).Inc() }

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

func (m *Metrics) observe(route string, status int, seconds float64) {
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Observe(seconds)
}
