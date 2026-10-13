package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"x402/internal/config"
	"x402/internal/problem"
	"x402/internal/registry"
	"x402/internal/store"
)

const (
	defaultDeadline = 5000 * time.Millisecond
	// nginx's code for a client that closed the request before the answer
	statusClientClosed = 499
	prefix             = "/internal/v1/chains/{chain}/x402"
)

var errNoData = errors.New("the indexer has produced no data yet")

type app struct {
	st     *store.Store
	ready  func() bool
	chains map[string]*chainApp
}

type chainApp struct {
	name         string
	id           int
	prefix       string
	ids          map[string]bool
	facilitators map[string]string
	registryJSON []byte
	coverage     []coverage
}

func (a *app) names() []string {
	return slices.Sorted(maps.Keys(a.chains))
}

var routes = []struct {
	pattern string
	handler func(*app) http.HandlerFunc
}{
	{"GET /healthz", (*app).healthz},
	{"GET /readyz", (*app).readyz},
	{"GET " + prefix + "/status", (*app).status},
	{"GET " + prefix + "/registry", (*app).registry},
	{"GET " + prefix + "/leaderboards/{kind}", (*app).leaderboards},
	{"GET " + prefix + "/flows", (*app).flows},
	{"GET " + prefix + "/rules/daily", (*app).rulesDaily},
	{"GET " + prefix + "/{kind}/{id}/daily", (*app).daily},
	{"GET " + prefix + "/payments", (*app).payments},
	{"GET " + prefix + "/payments/{tx}", (*app).tx},
}

func New(st *store.Store, reg *registry.Registry, cfgs []config.Config, m *Metrics, ready func() bool) (http.Handler, error) {
	a := &app{st: st, ready: ready, chains: map[string]*chainApp{}}
	for _, cfg := range cfgs {
		r, ok := reg.For(cfg.Chain)
		if !ok {
			return nil, fmt.Errorf("the registry has no entry for %s", cfg.Chain)
		}
		ch := &chainApp{
			name: cfg.Chain, id: cfg.ChainID, prefix: strconv.Itoa(cfg.ChainID) + "_", ids: map[string]bool{}, facilitators: map[string]string{},
			registryJSON: marshal(registryBody(cfg.Chain, r)), coverage: coverageFor(cfg),
		}
		for _, e := range r.Entries {
			ch.ids[e.ID] = true
			if e.Class == "facilitator" {
				ch.facilitators[e.ID] = e.Name
			}
		}
		a.chains[cfg.Chain] = ch
	}
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, rt.handler(a))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		problem.Write(w, problem.New(problem.CodeNotFound, "No such route.", req.URL.EscapedPath()))
	})
	return instrument(m, mux), nil
}

func marshal(v any) []byte {
	// AllowInvalidUTF8 leaves no encode error to handle
	b, _ := json.Marshal(v, jsontext.AllowInvalidUTF8(true))
	return b
}

/* Middleware */

func instrument(m *Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/healthz" || req.URL.Path == "/readyz" {
			next.ServeHTTP(w, req)
			return
		}
		start := time.Now()
		w.Header().Set("X-Request-Id", requestID(req.Header.Get("traceparent")))
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, req)
		route := req.Pattern
		if route == "" || route == "/" {
			route = "unmatched"
		}
		m.observe(route, sw.code(), time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func requestID(traceparent string) string {
	if len(traceparent) == 55 && traceparent[:3] == "00-" && traceparent[35] == '-' && traceparent[52] == '-' &&
		isLowerHex(traceparent[3:35]) && isLowerHex(traceparent[36:52]) && isLowerHex(traceparent[53:]) &&
		strings.Trim(traceparent[3:35], "0") != "" && strings.Trim(traceparent[36:52], "0") != "" {
		return traceparent[3:35]
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], rand.Uint64())
	binary.BigEndian.PutUint64(b[8:], rand.Uint64())
	return hex.EncodeToString(b[:])
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

/* Probes */

func (*app) healthz() http.HandlerFunc { return func(http.ResponseWriter, *http.Request) {} }

func (a *app) readyz() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !a.ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}
}

/* Serving */

type call struct {
	ch      *chainApp
	a       *app
	ctx     context.Context
	req     *http.Request
	meta    store.Meta
	stamped bool
}

type endpoint func(c *call) ([]byte, error)

func (c *call) data() (store.Meta, error) {
	m, err := c.a.st.Meta(c.ctx, c.ch.id)
	if err != nil {
		return m, err
	}
	if m.WatermarkBlock < 0 {
		return m, errNoData
	}
	c.meta, c.stamped = m, true
	return m, nil
}

// watermarkDay falls back to the newest daily day while the indexer has no block time to give
func (c *call) watermarkDay() (int, error) {
	if c.meta.ProgressTime > 0 {
		return int(c.meta.ProgressTime / 86400), nil
	}
	return c.a.st.LastDay(c.ctx, c.ch.id)
}

func (c *call) resolve(w window) (from, to int, err error) { return c.resolveUpTo(w, maxSpanDays) }

func (c *call) resolveUpTo(w window, maxDays int) (from, to int, err error) {
	day, err := c.watermarkDay()
	if err != nil {
		return 0, 0, err
	}
	return w.resolve(day, maxDays)
}

func (c *call) stamp(h http.Header) {
	h.Set("Sona-Finality", "finalized")
	h.Set("Sona-Block", strconv.FormatInt(c.meta.WatermarkBlock, 10))
	if c.meta.ProgressTime > 0 {
		h.Set("Sona-Block-Time", strconv.FormatInt(c.meta.ProgressTime, 10))
	}
}

func deadline(h http.Header) time.Duration {
	ms, err := strconv.ParseInt(h.Get("Sona-Deadline-Ms"), 10, 64)
	if err != nil {
		return defaultDeadline
	}
	const limit = int64(time.Hour / time.Millisecond)
	return time.Duration(max(min(ms, limit), -limit)) * time.Millisecond
}

func (a *app) serve(h endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		budget := deadline(req.Header)
		if budget <= 0 {
			a.fail(w, req, context.DeadlineExceeded)
			return
		}
		ch, err := a.chain(req.PathValue("chain"))
		if err != nil {
			a.fail(w, req, err)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), budget)
		defer cancel()
		c := &call{a: a, ch: ch, ctx: ctx, req: req}
		body, err := h(c)
		if err != nil {
			a.fail(w, req, err)
			return
		}
		hdr := w.Header()
		if c.stamped {
			c.stamp(hdr)
		}
		hdr.Set("Content-Type", "application/json")
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}
}

func (a *app) fail(w http.ResponseWriter, req *http.Request, err error) {
	instance := req.URL.EscapedPath()
	var he *httpError
	var p problem.Problem
	switch {
	case errors.As(err, &he):
		p = problem.New(he.code, he.detail, instance)
	case errors.Is(err, errNoData):
		p = problem.New(problem.CodeUpstream, "The indexer has produced no data yet.", instance)
	case errors.Is(err, context.DeadlineExceeded):
		p = problem.New(problem.CodeDeadline, "The deadline passed before the data source answered.", instance)
		a.log(w, req, err)
	case errors.Is(err, context.Canceled) && req.Context().Err() != nil:
		w.WriteHeader(statusClientClosed)
		return
	case unreachable(err):
		p = problem.New(problem.CodeUpstream, "The database did not answer.", instance)
		a.log(w, req, err)
	default:
		p = problem.New(problem.CodeInternal, "The request failed.", instance)
		a.log(w, req, err)
	}
	problem.Write(w, p)
}

func (*app) log(w http.ResponseWriter, req *http.Request, err error) {
	slog.ErrorContext(req.Context(), "request failed", "request_id", w.Header().Get("X-Request-Id"), "route", req.Pattern, "error", err)
}

// unreachable is true for every database error that is not a statement the server rejected
func unreachable(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return true
	}
	switch pg.Code[:min(2, len(pg.Code))] {
	case "08", "53", "57":
		return true
	}
	return false
}

/* Status and registry */

type coverage struct {
	Code string `json:"code"`
	Note string `json:"note"`
}

var coverageNotes = []coverage{
	{"batch_channels_not_indexed", "The batch scheme is not indexed, its deposits are labelled batch_deposit and left out of every total."},
	{"tier_b_is_shape_only", "Tier B holds every payment of the x402 shape from a relayer that is no listed facilitator, so it includes ordinary gasless transfers."},
	{"volume_usdc_only", "Volume counts USDC only, a payment in another token adds to the payment counts and adds 0 to volume."},
	{"reverted_settlements_not_visible", "A reverted settlement leaves no log, so failed attempts and failure rates cannot be seen."},
	{"upto_ceiling_not_captured", "The Upto scheme stores the settled amount, the signed ceiling is not captured."},
}

func coverageFor(cfg config.Config) []coverage {
	out := slices.Clone(coverageNotes)
	if cfg.HistoryFrom != "" {
		out = append(out, coverage{"history_starts", "The indexer covers this chain from " + cfg.HistoryFrom + ", earlier payments are not counted."})
	}
	if cfg.RetentionDays > 0 {
		out = append(out, coverage{"payment_rows_pruned", "Payment rows are kept for the last " + strconv.Itoa(cfg.RetentionDays) + " days, daily totals and rankings are kept without limit. Days older than that keep the totals computed under the rules in force at the time, and a payer's history for the rules reaches back only that far."})
	}
	return out
}

type statusBody struct {
	Kind            string      `json:"kind"`
	Chain           string      `json:"chain"`
	Indexer         indexerBody `json:"indexer"`
	Engine          engineBody  `json:"engine"`
	RegistryVersion string      `json:"registry_version"`
	RulesVersion    string      `json:"rules_version"`
	Rows            rowsBody    `json:"rows"`
	Coverage        []coverage  `json:"coverage"`
}

type indexerBody struct {
	Initialised  bool   `json:"initialised"`
	ProgressBlk  int64  `json:"progress_block"`
	SourceBlk    int64  `json:"source_block"`
	IsReady      bool   `json:"is_ready"`
	ProgressAgeS *int64 `json:"progress_age_s"`
}

type engineBody struct {
	CutoffBlk     int64   `json:"cutoff_block"`
	WatermarkBlk  int64   `json:"watermark_block"`
	WatermarkTime *string `json:"watermark_time"`
	LagBlocks     *int64  `json:"lag_blocks"`
	RefreshedAt   string  `json:"refreshed_at"`
	RefreshMS     int64   `json:"refresh_ms"`
}

type rowsBody struct {
	Payments int64 `json:"payments"`
	TierA    int64 `json:"tier_a"`
	TierB    int64 `json:"tier_b"`
	TierX    int64 `json:"tier_x"`
	Mints    int64 `json:"mints"`
}

func (a *app) status() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		if _, err := parseQuery(c.req); err != nil {
			return nil, err
		}
		m, err := a.st.Meta(c.ctx, c.ch.id)
		if err != nil {
			return nil, err
		}
		b := statusBody{
			Kind: "x402_status", Chain: c.ch.name,
			Indexer: indexerBody{Initialised: m.Initialised, ProgressBlk: m.IndexerProgressBlock, SourceBlk: m.SourceBlock, IsReady: m.IsReady},
			Engine: engineBody{
				CutoffBlk: m.CutoffBlock, WatermarkBlk: m.WatermarkBlock, RefreshedAt: m.RefreshedAt.UTC().Format(time.RFC3339), RefreshMS: m.RefreshMS,
			},
			RegistryVersion: m.RegistryVersion, RulesVersion: m.RulesVersion,
			Rows:     rowsBody{m.RowsPayment, m.RowsTierA, m.RowsTierB, m.RowsTierX, m.RowsMint},
			Coverage: c.ch.coverage,
		}
		if m.ProgressTime > 0 {
			age := max(time.Now().Unix()-m.ProgressTime, 0)
			at := time.Unix(m.ProgressTime, 0).UTC().Format(time.RFC3339)
			b.Indexer.ProgressAgeS, b.Engine.WatermarkTime = &age, &at
		}
		if m.Initialised && m.WatermarkBlock >= 0 {
			lag := m.IndexerProgressBlock - m.WatermarkBlock
			b.Engine.LagBlocks = &lag
		}
		return marshal(b), nil
	})
}

type facilitatorBody struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	URL          string   `json:"url,omitzero"`
	Addresses    []string `json:"addresses"`
	BuilderCodes []string `json:"builder_codes,omitzero"`
}

type relayerBody struct {
	Address       string  `json:"address"`
	Class         string  `json:"class"`
	FacilitatorID *string `json:"facilitator_id"`
	FromBlock     *int64  `json:"from_block"`
	ToBlock       *int64  `json:"to_block"`
}

type registryResponse struct {
	Kind         string            `json:"kind"`
	Chain        string            `json:"chain"`
	Version      string            `json:"version"`
	Checksum     string            `json:"checksum"`
	Facilitators []facilitatorBody `json:"facilitators"`
	Relayers     []relayerBody     `json:"relayers"`
}

func registryBody(chain string, reg *registry.Registry) registryResponse {
	b := registryResponse{
		Kind: "x402_registry", Chain: chain, Version: reg.Version, Checksum: reg.Checksum,
		Facilitators: []facilitatorBody{}, Relayers: []relayerBody{},
	}
	for _, e := range reg.Entries {
		var id *string
		if e.Class == "facilitator" {
			id = &e.ID
			f := facilitatorBody{ID: e.ID, Name: e.Name, URL: e.URL, Addresses: make([]string, 0, len(e.Addresses)), BuilderCodes: e.Codes}
			for _, a := range e.Addresses {
				f.Addresses = append(f.Addresses, a.Hex)
			}
			b.Facilitators = append(b.Facilitators, f)
		}
		for _, a := range e.Addresses {
			b.Relayers = append(b.Relayers, relayerBody{Address: a.Hex, Class: e.Class, FacilitatorID: id, FromBlock: a.FromBlock, ToBlock: a.ToBlock})
		}
	}
	return b
}

func (a *app) registry() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		if _, err := parseQuery(c.req); err != nil {
			return nil, err
		}
		return c.ch.registryJSON, nil
	})
}
