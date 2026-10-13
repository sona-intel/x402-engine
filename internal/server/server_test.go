package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"x402/internal/config"
	"x402/internal/refresh"
	"x402/internal/registry"
	"x402/internal/store"
	"x402/internal/testkit"
)

const (
	base           = "/internal/v1/chains/eip155:143/x402"
	molandak       = "0x7f6a2850669202519f0fe8aa912451238820db86"
	unlisted       = "0x74dd46f2e70b964895fe21aa70c60a9b61fbe395"
	testRelayer    = "0x22c4953c882264cab154ea688004e6dead21eeeb"
	acrossRelayer  = "0x07ae8551be970cb1cca11dd7a11f47ae82e70e67"
	burstSeller    = "0x1995159c8d4df2268a17c0169a80f0e7d11a7424"
	strayPayee     = "0x93053f1e7a5efeda532fe69cbbe43cbec3a0f13f"
	selfPayer      = "0x940ecb27d8f4f7009c8bc12969496e9639717638"
	minter         = "0x99f5a2e5d81f22ddb130f4a194584532a3dc3c2e"
	otherToken     = "0x00000000efe302beaa2b3e6e1b18d08d69a9012a"
	pagingPayer    = "0x00000000000000000000000000000000000000b1"
	ausdPayer      = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	upperCasePayer = "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
	ausdTime       = 1770136004
	maxUint256     = "115792089237316195423570985008687907853269984665640564039457584007913129639935"
	gasCost        = "15022866000000000"
	progress       = 6000
	cutoff         = progress - 10
	unknownTx      = "0x00000000000000000000000000000000000000000000000000000000000000ff"
)

var (
	windowSeller = testkit.Addr(77)
	twoTx        = testkit.Hash(4200)
	lateTx       = testkit.Hash(5995)
	settleTx     = testkit.Hash(3400)
	windowDays   = []int64{20, 10, 2}
)

type world struct {
	h        http.Handler
	d        *testkit.DB
	reg      *registry.Registry
	cfg      config.Config
	day      int64
	progTime int64
	served   int
}

func load(t *testing.T) (*testkit.DB, config.Config, *registry.Registry) {
	t.Helper()
	d := testkit.New(t)
	d.Migrate(t)
	cfgs, err := config.Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgs[0]
	cfg.SourceSchema = d.Source
	reg, err := registry.Load("../../config/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return d, cfg, reg
}

func handlerFor(t testing.TB, st *store.Store, reg *registry.Registry, cfgs []config.Config, m *Metrics, ready func() bool) http.Handler {
	t.Helper()
	h, err := New(st, reg, cfgs, m, ready)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func dayTime(day, secs int64) int64 { return day*86400 + secs }

func upperHex(a string) string { return "0x" + strings.ToUpper(a[2:]) }

func usdc(r testkit.Row) testkit.Row {
	r.Method = cmp.Or(r.Method, "eip3009_transfer")
	r.Token = cmp.Or(r.Token, testkit.USDC)
	r.TxTo = cmp.Or(r.TxTo, r.Token)
	r.Selector = cmp.Or(r.Selector, "0xe3ee160e")
	r.Relayer = cmp.Or(r.Relayer, molandak)
	r.GasCost = gasCost
	r.Nonce = testkit.Hash(int(r.Block)*10 + r.LogIndex)
	return r
}

func seedRows(day int64) []testkit.Row {
	rows := testkit.Burst(1000, 1518, testkit.BurstOpts{Payers: 56, Payee: burstSeller, Relayer: molandak, Amount: "100", BlockTime: dayTime(day-1, 100)})
	for i := range rows {
		rows[i].GasCost = gasCost
	}
	for i, off := range windowDays {
		rows = append(rows, usdc(testkit.Row{
			Block: int64(2600 + i), LogIndex: 1, BlockTime: dayTime(day-off, 3600), Tx: testkit.Hash(2600 + i),
			Payer: testkit.Addr(200 + i), Payee: windowSeller, Amount: "40",
		}))
	}
	rows = append(rows,
		usdc(testkit.Row{Block: 3000, LogIndex: 1, BlockTime: dayTime(day-4, 50), Tx: testkit.Hash(3000), Payer: testkit.Addr(1030), Payee: testkit.Addr(1031), Amount: "1000000", Relayer: unlisted}),
		usdc(testkit.Row{Block: 3100, LogIndex: 1, BlockTime: dayTime(day-6, 50), Tx: testkit.Hash(3100), Payer: testkit.Addr(1032), Payee: strayPayee, Amount: "10000"}),
		usdc(testkit.Row{Block: 3200, LogIndex: 1, BlockTime: dayTime(day-3, 50), Tx: testkit.Hash(3200), Payer: testkit.Addr(1033), Payee: strayPayee, Amount: "10000"}),
		usdc(testkit.Row{Block: 3300, LogIndex: 1, BlockTime: dayTime(day-5, 50), Tx: testkit.Hash(3300), Payer: selfPayer, Payee: selfPayer, Amount: "1000"}),
		usdc(testkit.Row{
			Block: 3400, LogIndex: 1, BlockTime: dayTime(day-7, 50), Tx: settleTx, Method: "permit2_exact", Payer: testkit.Addr(1034), Payee: testkit.Addr(1035),
			Amount: "1000", Relayer: testRelayer, Selector: "0x13cd3b53", ValidAfter: "1791209657", ValidBefore: "1791213857",
		}),
		testkit.Row{
			Block: 3500, LogIndex: 110, BlockTime: dayTime(day-8, 50), Tx: testkit.Hash(3500), Method: "other", Token: testkit.USDC, Payer: testkit.Addr(1036),
			Nonce: testkit.Hash(35000), Relayer: acrossRelayer, TxTo: "0x998d7c178a1f9607b47ea3a8e22426451b3ce707", Selector: "0xe3a64e34", GasCost: "56468070400000000",
		},
		usdc(testkit.Row{
			Block: 4000, LogIndex: 1, BlockTime: ausdTime, Tx: testkit.Hash(4000), Token: otherToken, Payer: ausdPayer, Payee: testkit.Addr(1009),
			Amount: maxUint256, ValidAfter: "0", ValidBefore: "1791213857",
		}),
		testkit.Row{Block: 4100, LogIndex: 1, BlockTime: dayTime(day-9, 50), Tx: testkit.Hash(4100), Method: "permit2_upto", Token: testkit.USDC, Payer: testkit.Addr(60)},
		usdc(testkit.Row{Block: 4200, LogIndex: 1, BlockTime: dayTime(day-10, 50), Tx: twoTx, Payer: testkit.Addr(70), Payee: testkit.Addr(72), Amount: "5"}),
		usdc(testkit.Row{Block: 4200, LogIndex: 2, BlockTime: dayTime(day-10, 50), Tx: twoTx, Payer: testkit.Addr(71), Payee: testkit.Addr(72), Amount: "6"}),
		usdc(testkit.Row{Block: 5995, LogIndex: 1, BlockTime: dayTime(day, 50), Tx: lateTx, Payer: testkit.Addr(80), Payee: testkit.Addr(81), Amount: "7"}))
	for b := range int64(40) {
		for l := 1; l <= 3; l++ {
			rows = append(rows, usdc(testkit.Row{
				Block: 5000 + b, LogIndex: l, BlockTime: dayTime(day-11, 100) + b/4, Tx: testkit.Hash(int(5000 + b)),
				Payer: pagingPayer, Payee: testkit.Addr(int(300 + b*3 + int64(l))), Amount: "1",
			}))
		}
	}
	return rows
}

func build(t *testing.T) *world {
	t.Helper()
	d, cfg, reg := load(t)
	progTime := time.Now().Unix() - 60
	d.SetProgress(t, progress, progTime)
	rows := seedRows(progTime / 86400)
	d.Insert(t, rows...)
	mints := make([]testkit.Mint, 51)
	for i := range mints {
		mints[i] = testkit.Mint{Block: int64(500 + i), LogIndex: 1, BlockTime: progTime - 86400, Tx: testkit.Hash(7000 + i), Recipient: testkit.Addr(i), Relayer: minter, Amount: "480379"}
	}
	d.InsertMints(t, mints...)
	if _, err := refresh.Once(t.Context(), d.Store, cfg, reg); err != nil {
		t.Fatal(err)
	}
	return &world{
		h: handlerFor(t, d.Store, reg, []config.Config{cfg}, NewMetrics(), func() bool { return true }), d: d, reg: reg, cfg: cfg,
		day: progTime / 86400, progTime: progTime, served: len(rows) - 1,
	}
}

func seeded(t *testing.T) http.Handler { return build(t).h }

type reply struct {
	code   int
	header http.Header
	raw    []byte
	body   map[string]any
}

func get(t *testing.T, h http.Handler, path string, headers ...string) reply {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	r := reply{code: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
	if len(r.raw) > 0 {
		if err := json.Unmarshal(r.raw, &r.body); err != nil {
			t.Fatalf("GET %s: body is not a JSON object: %v\n%s", path, err, r.raw)
		}
	}
	return r
}

func ok(t *testing.T, h http.Handler, path string) reply {
	t.Helper()
	r := get(t, h, path)
	if r.code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200\n%s", path, r.code, r.raw)
	}
	return r
}

func itemsOf(t *testing.T, r reply) []map[string]any {
	t.Helper()
	raw, isList := r.body["items"].([]any)
	if !isList {
		t.Fatalf("items is %T in %s", r.body["items"], r.raw)
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func firstItem(t *testing.T, r reply) map[string]any {
	t.Helper()
	items := itemsOf(t, r)
	if len(items) == 0 {
		t.Fatalf("no items in %s", r.raw)
	}
	return items[0]
}

func wantProblem(t *testing.T, r reply, status int, code string) {
	t.Helper()
	if r.code != status || r.body["code"] != code {
		t.Fatalf("got %d %v, want %d %s\n%s", r.code, r.body["code"], status, code, r.raw)
	}
	if r.header.Get("Content-Type") != "application/problem+json" || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("problem headers %q %q", r.header.Get("Content-Type"), r.header.Get("Cache-Control"))
	}
	if r.body["type"] != "urn:sona:problem:"+code || r.body["status"] != float64(status) {
		t.Errorf("problem body %s", r.raw)
	}
}

func intOf(t *testing.T, v any) int64 {
	t.Helper()
	f, isNumber := v.(float64)
	if !isNumber {
		t.Fatalf("%v (%T) is not a number", v, v)
	}
	return int64(f)
}

func scalar[T any](t *testing.T, d *testkit.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := d.Store.Pool.QueryRow(t.Context(), d.SQL(q), args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func dayString(day int64) string { return time.Unix(day*86400, 0).UTC().Format(time.DateOnly) }

func TestPaymentFields(t *testing.T) {
	p := firstItem(t, get(t, seeded(t), base+"/payments?payer="+upperCasePayer+"&tier=all"))
	t.Run("every_key_is_present", func(t *testing.T) {
		for _, k := range []string{
			"id", "chain", "block", "block_time", "tx", "log_index", "method", "token", "payer", "payee", "amount",
			"nonce", "valid_after", "valid_before", "relayer", "tx_to", "selector", "gas_cost", "tier", "facilitator", "reason", "genuine", "rules_fired",
		} {
			if _, ok := p[k]; !ok {
				t.Errorf("missing key %s", k)
			}
		}
	})
	t.Run("amount_keeps_78_digits", func(t *testing.T) {
		if p["amount"] != "115792089237316195423570985008687907853269984665640564039457584007913129639935" {
			t.Errorf("amount %v", p["amount"])
		}
	})
	t.Run("block_time_is_rfc3339_utc", func(t *testing.T) {
		if p["block_time"] != "2026-02-03T16:26:44Z" {
			t.Errorf("block_time %v", p["block_time"])
		}
	})
	t.Run("values_are_lowercase_hex_and_decimal_strings", func(t *testing.T) {
		want := map[string]any{
			"id": "143_4000_1", "chain": "eip155:143", "block": 4000.0, "log_index": 1.0, "method": "eip3009_transfer", "token": otherToken,
			"payer": ausdPayer, "payee": testkit.Addr(1009), "tx": testkit.Hash(4000), "relayer": molandak, "tx_to": otherToken, "selector": "0xe3ee160e",
			"valid_after": "0", "valid_before": "1791213857", "gas_cost": gasCost, "tier": "A", "facilitator": "monad-foundation", "reason": nil, "genuine": true,
		}
		for k, v := range want {
			if p[k] != v {
				t.Errorf("%s = %v (%T), want %v", k, p[k], p[k], v)
			}
		}
		if fired, isList := p["rules_fired"].([]any); !isList || len(fired) != 0 {
			t.Errorf("rules_fired = %v", p["rules_fired"])
		}
	})
}

func TestPaymentCarriesNullsAndVerdicts(t *testing.T) {
	h := seeded(t)
	t.Run("a_row_without_relayer_or_payee_keeps_every_key_as_null", func(t *testing.T) {
		p := firstItem(t, get(t, h, base+"/payments?payer="+testkit.Addr(60)+"&tier=AB"))
		for _, k := range []string{"payee", "amount", "relayer", "tx_to", "selector", "gas_cost", "nonce", "valid_after", "valid_before", "reason"} {
			if v, present := p[k]; !present || v != nil {
				t.Errorf("%s = %v (present %v), want null", k, v, present)
			}
		}
		if p["tier"] != "B" || p["facilitator"] != "unknown" || p["method"] != "permit2_upto" || p["genuine"] != true {
			t.Errorf("row %v", p)
		}
	})
	t.Run("a_tier_x_row_has_a_null_verdict_and_a_reason", func(t *testing.T) {
		p := firstItem(t, get(t, h, base+"/payments?payer="+testkit.Addr(1036)+"&tier=all"))
		if p["tier"] != "X" || p["genuine"] != nil || p["reason"] != "not_x402" || p["facilitator"] != nil || p["method"] != "other" || p["id"] != "143_3500_110" {
			t.Errorf("row %v", p)
		}
	})
	t.Run("a_burst_row_lists_the_signals_that_fired", func(t *testing.T) {
		p := firstItem(t, get(t, h, base+"/payments?payee="+burstSeller+"&limit=1"))
		fired, _ := p["rules_fired"].([]any)
		if p["genuine"] != false || len(fired) < 3 || fired[0] != "fixed_amount" || fired[1] != "payee_concentration" || fired[2] != "no_other_activity" {
			t.Errorf("genuine %v rules_fired %v", p["genuine"], p["rules_fired"])
		}
	})
	t.Run("a_self_payment_fires_self_payment", func(t *testing.T) {
		p := firstItem(t, get(t, h, base+"/payments?payer="+selfPayer))
		if fired, _ := p["rules_fired"].([]any); p["genuine"] != false || !slices.Equal(fired, []any{"self_payment"}) {
			t.Errorf("genuine %v rules_fired %v", p["genuine"], p["rules_fired"])
		}
	})
}

type gotStatus struct {
	Kind    string `json:"kind"`
	Chain   string `json:"chain"`
	Indexer struct {
		Initialised  bool   `json:"initialised"`
		ProgressBlk  int64  `json:"progress_block"`
		SourceBlk    int64  `json:"source_block"`
		IsReady      bool   `json:"is_ready"`
		ProgressAgeS *int64 `json:"progress_age_s"`
	} `json:"indexer"`
	Engine struct {
		CutoffBlk     int64   `json:"cutoff_block"`
		WatermarkBlk  int64   `json:"watermark_block"`
		WatermarkTime *string `json:"watermark_time"`
		LagBlocks     *int64  `json:"lag_blocks"`
		RefreshedAt   string  `json:"refreshed_at"`
		RefreshMS     int64   `json:"refresh_ms"`
	} `json:"engine"`
	RegistryVersion string `json:"registry_version"`
	RulesVersion    string `json:"rules_version"`
	Rows            struct {
		Payments int64 `json:"payments"`
		TierA    int64 `json:"tier_a"`
		TierB    int64 `json:"tier_b"`
		TierX    int64 `json:"tier_x"`
		Mints    int64 `json:"mints"`
	} `json:"rows"`
	Coverage []struct {
		Code string `json:"code"`
		Note string `json:"note"`
	} `json:"coverage"`
}

func TestStatus(t *testing.T) {
	w := build(t)
	r := ok(t, w.h, base+"/status")
	var s gotStatus
	if err := json.Unmarshal(r.raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Kind != "x402_status" || s.Chain != "eip155:143" || !s.Indexer.Initialised || !s.Indexer.IsReady {
		t.Errorf("head %+v", s)
	}
	if s.Indexer.ProgressBlk != progress || s.Indexer.SourceBlk != progress+3 {
		t.Errorf("indexer blocks %d %d", s.Indexer.ProgressBlk, s.Indexer.SourceBlk)
	}
	if s.Indexer.ProgressAgeS == nil || *s.Indexer.ProgressAgeS < 60 || *s.Indexer.ProgressAgeS > 120 {
		t.Errorf("progress_age_s %v", s.Indexer.ProgressAgeS)
	}
	if s.Engine.CutoffBlk != cutoff || s.Engine.WatermarkBlk != cutoff || s.Engine.LagBlocks == nil || *s.Engine.LagBlocks != progress-cutoff {
		t.Errorf("engine %+v", s.Engine)
	}
	if want := time.Unix(w.progTime, 0).UTC().Format(time.RFC3339); s.Engine.WatermarkTime == nil || *s.Engine.WatermarkTime != want {
		t.Errorf("watermark_time %v, want %s", s.Engine.WatermarkTime, want)
	}
	if at, err := time.Parse(time.RFC3339, s.Engine.RefreshedAt); err != nil || time.Since(at) > time.Minute {
		t.Errorf("refreshed_at %q: %v", s.Engine.RefreshedAt, err)
	}
	if s.RegistryVersion != "2026-10-12.1" || s.RulesVersion != "2026-10-12.1" {
		t.Errorf("versions %q %q", s.RegistryVersion, s.RulesVersion)
	}
	if s.Rows.Payments != int64(w.served) || s.Rows.TierA+s.Rows.TierB+s.Rows.TierX != s.Rows.Payments || s.Rows.Mints != 51 || s.Rows.TierX != 2 || s.Rows.TierB != 2 {
		t.Errorf("rows %+v, served %d", s.Rows, w.served)
	}
	var codes []string
	for _, c := range s.Coverage {
		if c.Note == "" {
			t.Errorf("coverage %s has no note", c.Code)
		}
		codes = append(codes, c.Code)
	}
	want := []string{"batch_channels_not_indexed", "tier_b_is_shape_only", "volume_usdc_only", "reverted_settlements_not_visible", "upto_ceiling_not_captured"}
	if !slices.Equal(codes, want) {
		t.Errorf("coverage %v", codes)
	}
	if r.header.Get("X-Request-Id") == "" || r.header.Get("Sona-Block") != "" {
		t.Errorf("status headers %v", r.header)
	}
}

func TestRegistry(t *testing.T) {
	w := build(t)
	r := ok(t, w.h, base+"/registry")
	if r.body["kind"] != "x402_registry" || r.body["chain"] != "eip155:143" || r.body["version"] != w.reg.Version || r.body["checksum"] != w.reg.Checksum {
		t.Fatalf("head %s", r.raw)
	}
	var ids []string
	for _, f := range r.body["facilitators"].([]any) {
		f := f.(map[string]any)
		ids = append(ids, f["id"].(string))
		if f["name"] == "" || f["addresses"] == nil {
			t.Errorf("facilitator %v", f)
		}
	}
	if !slices.Equal(ids, []string{"monad-foundation", "openx402", "dexter", "pieverse"}) {
		t.Errorf("facilitator ids %v", ids)
	}
	var found map[string]any
	relayers := r.body["relayers"].([]any)
	for _, rel := range relayers {
		if rel.(map[string]any)["address"] == unlisted {
			found = rel.(map[string]any)
		}
	}
	if found == nil || found["class"] != "unlisted" || found["facilitator_id"] != nil || found["from_block"] != nil || found["to_block"] != nil {
		t.Errorf("unlisted relayer %v", found)
	}
	if len(relayers) != 12 {
		t.Errorf("%d relayers, want 12", len(relayers))
	}
}

func TestBeforeTheIndexerExists(t *testing.T) {
	d, cfg, reg := load(t)
	var done atomic.Bool
	h := handlerFor(t, d.Store, reg, []config.Config{cfg}, NewMetrics(), done.Load)
	paths := []string{"/leaderboards/facilitators", "/facilitators/monad-foundation/daily", "/payments", "/payments/" + unknownTx}
	check := func(t *testing.T) {
		t.Helper()
		for _, p := range paths {
			r := get(t, h, base+p)
			wantProblem(t, r, http.StatusServiceUnavailable, "upstream_unavailable")
			if r.header.Get("Retry-After") != "1" {
				t.Errorf("%s: Retry-After %q", p, r.header.Get("Retry-After"))
			}
		}
	}
	if r := get(t, h, "/readyz"); r.code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before the first refresh = %d", r.code)
	}
	t.Run("before_any_refresh", check)
	res, err := refresh.Once(t.Context(), d.Store, cfg, reg)
	if err != nil || !res.NotInitialised {
		t.Fatalf("Once = %+v, %v", res, err)
	}
	done.Store(true)
	if r := get(t, h, "/readyz"); r.code != http.StatusOK {
		t.Fatalf("readyz after the first refresh = %d", r.code)
	}
	t.Run("after_a_refresh_that_found_no_indexer", check)
	var s gotStatus
	r := ok(t, h, base+"/status")
	if err := json.Unmarshal(r.raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Indexer.Initialised || s.Engine.WatermarkBlk != -1 || s.Rows.Payments != 0 || len(s.Coverage) != 5 {
		t.Errorf("status %+v", s)
	}
	if r := get(t, h, base+"/registry"); r.code != http.StatusOK {
		t.Errorf("registry = %d", r.code)
	}
}

func TestLeaderboardFacilitators(t *testing.T) {
	w := build(t)
	from, to := w.day-29, w.day
	var raw, rawVol, gen, genVol, gas string
	err := w.d.Store.Pool.QueryRow(t.Context(), `
		select count(*)::text, coalesce(sum(amount) filter (where token = decode(substr($3::text, 3), 'hex')), 0)::text, (count(*) filter (where genuine))::text,
			coalesce(sum(amount) filter (where token = decode(substr($3::text, 3), 'hex') and genuine), 0)::text, coalesce(sum(gas_cost), 0)::text
		from payment_signals where tier = 'A' and facilitator_id = 'monad-foundation' and day between $1 and $2`, from, to, testkit.USDC).
		Scan(&raw, &rawVol, &gen, &genVol, &gas)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "0" || gen == "0" || genVol == "0" {
		t.Fatalf("the seed does not exercise the aggregate: %s %s %s %s", raw, rawVol, gen, genVol)
	}
	check := func(t *testing.T, p map[string]any) {
		t.Helper()
		if p["id"] != "monad-foundation" || p["name"] != "Molandak" || p["tier"] != "A" {
			t.Fatalf("item %v", p)
		}
		if strconv.FormatInt(intOf(t, p["raw_payments"]), 10) != raw || p["raw_volume"] != rawVol || strconv.FormatInt(intOf(t, p["genuine_payments"]), 10) != gen ||
			p["genuine_volume"] != genVol || p["gas_cost"] != gas {
			t.Errorf("item %v, want %s %s %s %s %s", p, raw, rawVol, gen, genVol, gas)
		}
	}
	t.Run("default_order_and_tier", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/facilitators")
		if r.body["kind"] != "x402_leaderboard" || r.body["chain"] != "eip155:143" || r.body["board"] != "facilitators" || r.body["tier"] != "A" ||
			r.body["order"] != "genuine_volume" || r.body["from"] != dayString(from) || r.body["to"] != dayString(to) {
			t.Fatalf("envelope %s", r.raw)
		}
		check(t, firstItem(t, r))
	})
	for _, order := range []string{"raw_volume", "raw_payments", "genuine_payments"} {
		t.Run("order_"+order, func(t *testing.T) {
			r := ok(t, w.h, base+"/leaderboards/facilitators?order="+order)
			check(t, firstItem(t, r))
			if r.body["order"] != order {
				t.Errorf("order %v", r.body["order"])
			}
		})
	}
	t.Run("tier_AB_adds_the_unlisted_relayer_as_tier_B", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/facilitators?tier=AB")
		var b map[string]any
		for _, p := range itemsOf(t, r) {
			if p["id"] == unlisted {
				b = p
			}
		}
		if b == nil || b["tier"] != "B" || b["name"] != nil {
			t.Fatalf("tier B item %v in %s", b, r.raw)
		}
		if _, present := b["name"]; !present {
			t.Error("name key missing on a tier B item")
		}
		if b["raw_payments"] != 1.0 || b["raw_volume"] != "1000000" || b["genuine_volume"] != "1000000" {
			t.Errorf("tier B item %v", b)
		}
	})
	t.Run("limit_one", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/facilitators?tier=AB&limit=1")
		if n := len(itemsOf(t, r)); n != 1 {
			t.Errorf("%d items", n)
		}
	})
}

func TestLeaderboardSellersAndPayers(t *testing.T) {
	w := build(t)
	first := func(t *testing.T, path string) map[string]any {
		return firstItem(t, ok(t, w.h, base+path))
	}
	t.Run("order_decides_the_first_seller", func(t *testing.T) {
		for order, want := range map[string]string{
			"genuine_volume": strayPayee, "genuine_payments": windowSeller, "raw_volume": burstSeller, "raw_payments": burstSeller,
		} {
			if p := first(t, "/leaderboards/sellers?order="+order); p["id"] != want {
				t.Errorf("order %s: first seller %v, want %s", order, p["id"], want)
			}
		}
	})
	t.Run("seller_items_have_no_name_and_no_gas", func(t *testing.T) {
		p := first(t, "/leaderboards/sellers?order=raw_volume")
		for _, k := range []string{"name", "gas_cost"} {
			if _, present := p[k]; present {
				t.Errorf("seller item has %s", k)
			}
		}
		if p["raw_payments"] != 1518.0 || p["raw_volume"] != "151800" || p["genuine_payments"] != 0.0 || p["genuine_volume"] != "0" || p["active_days"] != 1.0 {
			t.Errorf("burst seller %v", p)
		}
	})
	t.Run("payers_come_back_by_address", func(t *testing.T) {
		p := first(t, "/leaderboards/payers?order=raw_payments")
		if id, _ := p["id"].(string); !strings.HasPrefix(id, "0x") || len(id) != 42 {
			t.Errorf("payer id %v", p["id"])
		}
		if p["raw_payments"] != 120.0 || p["id"] != pagingPayer {
			t.Errorf("payer %v", p)
		}
	})
	t.Run("a_payment_in_another_token_adds_no_volume", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/payers?from=2026-02-03&to=2026-02-03&order=raw_volume")
		if p := firstItem(t, r); p["id"] != ausdPayer || p["raw_volume"] != "0" || p["raw_payments"] != 1.0 || p["genuine_payments"] != 1.0 {
			t.Errorf("AUSD payer adds payments and no volume: %v", p)
		}
	})
	t.Run("a_366_day_span_is_allowed", func(t *testing.T) {
		ok(t, w.h, base+"/leaderboards/payers?from=2025-01-01&to=2026-01-01")
	})
}

func TestLeaderboardWindow(t *testing.T) {
	w := build(t)
	seller := func(t *testing.T, query string) (map[string]any, reply) {
		t.Helper()
		r := ok(t, w.h, base+"/leaderboards/sellers?limit=100&"+query)
		for _, p := range itemsOf(t, r) {
			if p["id"] == windowSeller {
				return p, r
			}
		}
		return nil, r
	}
	t.Run("the_default_window_is_30_days_ending_at_the_watermark_day", func(t *testing.T) {
		p, r := seller(t, "")
		if r.body["from"] != dayString(w.day-29) || r.body["to"] != dayString(w.day) {
			t.Errorf("window %v..%v", r.body["from"], r.body["to"])
		}
		if p == nil || p["active_days"] != 3.0 || p["first_day"] != dayString(w.day-20) || p["last_day"] != dayString(w.day-2) || p["raw_payments"] != 3.0 {
			t.Errorf("seller %v", p)
		}
	})
	t.Run("from_and_to_are_inclusive", func(t *testing.T) {
		p, _ := seller(t, "from="+dayString(w.day-20)+"&to="+dayString(w.day-2))
		if p == nil || p["active_days"] != 3.0 {
			t.Errorf("both ends in: %v", p)
		}
		p, _ = seller(t, "from="+dayString(w.day-19)+"&to="+dayString(w.day-2))
		if p == nil || p["active_days"] != 2.0 || p["first_day"] != dayString(w.day-10) {
			t.Errorf("first end out: %v", p)
		}
		p, _ = seller(t, "from="+dayString(w.day-20)+"&to="+dayString(w.day-3))
		if p == nil || p["active_days"] != 2.0 || p["last_day"] != dayString(w.day-10) {
			t.Errorf("last end out: %v", p)
		}
		p, _ = seller(t, "from="+dayString(w.day-10)+"&to="+dayString(w.day-10))
		if p == nil || p["active_days"] != 1.0 || p["first_day"] != p["last_day"] {
			t.Errorf("one day: %v", p)
		}
	})
	t.Run("a_window_without_rows_is_empty", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/sellers?from=2020-01-01&to=2020-01-31")
		if items := itemsOf(t, r); len(items) != 0 {
			t.Errorf("%d items", len(items))
		}
	})
	t.Run("only_to_counts_back_30_days", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/sellers?to="+dayString(w.day-10))
		if r.body["from"] != dayString(w.day-39) {
			t.Errorf("from %v", r.body["from"])
		}
	})
	t.Run("without_a_block_time_the_window_ends_at_the_newest_day", func(t *testing.T) {
		w := build(t)
		if _, err := w.d.Store.Pool.Exec(t.Context(), `update meta set progress_time = 0`); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"/leaderboards/sellers", "/facilitators/monad-foundation/daily"} {
			r := ok(t, w.h, base+p)
			if r.body["to"] != dayString(w.day-1) || r.body["from"] != dayString(w.day-30) {
				t.Errorf("%s window %v..%v", p, r.body["from"], r.body["to"])
			}
		}
		if items := itemsOf(t, ok(t, w.h, base+"/leaderboards/sellers")); len(items) == 0 {
			t.Error("the leaderboard is empty")
		}
		if h := get(t, w.h, base+"/payments").header; h.Get("Sona-Block-Time") != "" || h.Get("Sona-Block") == "" {
			t.Errorf("headers %v", h)
		}
	})
	t.Run("only_from_runs_to_the_watermark_day", func(t *testing.T) {
		r := ok(t, w.h, base+"/leaderboards/sellers?from="+dayString(w.day-3))
		if r.body["to"] != dayString(w.day) {
			t.Errorf("to %v", r.body["to"])
		}
	})
}

func TestDaily(t *testing.T) {
	w := build(t)
	t.Run("a_facilitator_carries_payers_sellers_and_gas", func(t *testing.T) {
		r := ok(t, w.h, base+"/facilitators/monad-foundation/daily")
		if r.body["kind"] != "x402_daily" || r.body["chain"] != "eip155:143" || r.body["entity"] != "facilitators" || r.body["id"] != "monad-foundation" ||
			r.body["tier"] != "A" || r.body["from"] != dayString(w.day-29) || r.body["to"] != dayString(w.day) {
			t.Fatalf("envelope %s", r.raw)
		}
		items := itemsOf(t, r)
		if len(items) < 2 {
			t.Fatalf("%d items", len(items))
		}
		for _, p := range items {
			for _, k := range []string{"day", "tier", "raw_payments", "raw_volume", "genuine_payments", "genuine_volume", "raw_payers", "raw_sellers", "gas_cost"} {
				if _, present := p[k]; !present {
					t.Errorf("item lacks %s: %v", k, p)
				}
			}
		}
		for i := 1; i < len(items); i++ {
			if items[i-1]["day"].(string) >= items[i]["day"].(string) {
				t.Errorf("days not ascending: %v then %v", items[i-1]["day"], items[i]["day"])
			}
		}
		var burst map[string]any
		for _, p := range items {
			if p["day"] == dayString(w.day-1) {
				burst = p
			}
		}
		if burst == nil || burst["raw_payments"] != 1518.0 || burst["raw_volume"] != "151800" || burst["genuine_payments"] != 0.0 || burst["raw_payers"] != 56.0 || burst["raw_sellers"] != 1.0 {
			t.Errorf("burst day %v", burst)
		}
	})
	t.Run("a_seller_has_payers_and_no_sellers", func(t *testing.T) {
		r := ok(t, w.h, base+"/sellers/"+burstSeller+"/daily")
		p := firstItem(t, r)
		if _, present := p["raw_sellers"]; present {
			t.Error("seller day has raw_sellers")
		}
		if _, present := p["gas_cost"]; present {
			t.Error("seller day has gas_cost")
		}
		if p["raw_payers"] != 56.0 || p["raw_payments"] != 1518.0 {
			t.Errorf("seller day %v", p)
		}
	})
	t.Run("a_payer_has_sellers_and_no_payers", func(t *testing.T) {
		p := firstItem(t, ok(t, w.h, base+"/payers/"+upperHex(pagingPayer)+"/daily"))
		if _, present := p["raw_payers"]; present {
			t.Error("payer day has raw_payers")
		}
		if p["raw_sellers"] == nil {
			t.Errorf("payer day %v", p)
		}
	})
	t.Run("an_unknown_entity_is_404", func(t *testing.T) {
		wantProblem(t, get(t, w.h, base+"/sellers/"+testkit.Addr(9999)+"/daily"), http.StatusNotFound, "not_found")
		wantProblem(t, get(t, w.h, base+"/facilitators/dexter/daily"), http.StatusNotFound, "not_found")
	})
	t.Run("a_known_entity_with_only_tier_B_rows_is_200", func(t *testing.T) {
		if items := itemsOf(t, ok(t, w.h, base+"/facilitators/unknown/daily")); len(items) != 0 {
			t.Errorf("%d tier A days", len(items))
		}
		if p := firstItem(t, ok(t, w.h, base+"/facilitators/unknown/daily?tier=AB")); p["tier"] != "B" {
			t.Errorf("day %v", p)
		}
	})
	t.Run("a_known_entity_outside_the_window_is_200_with_empty_items", func(t *testing.T) {
		r := ok(t, w.h, base+"/sellers/"+burstSeller+"/daily?from=2020-01-01&to=2020-01-31")
		if items := itemsOf(t, r); len(items) != 0 {
			t.Errorf("%d items", len(items))
		}
		if !strings.Contains(string(r.raw), `"items":[]`) {
			t.Errorf("empty items must be an array: %s", r.raw)
		}
	})
	t.Run("tier_B_days_are_served_with_tier_AB", func(t *testing.T) {
		r := ok(t, w.h, base+"/facilitators/"+unlisted+"/daily?tier=AB")
		p := firstItem(t, r)
		if p["tier"] != "B" || p["raw_payments"] != 1.0 {
			t.Errorf("tier B day %v", p)
		}
		if items := itemsOf(t, ok(t, w.h, base+"/facilitators/"+unlisted+"/daily")); len(items) != 0 {
			t.Errorf("%d tier A days", len(items))
		}
	})
}

func TestPaymentsPaging(t *testing.T) {
	w := build(t)
	type page struct {
		ids    []string
		blocks [][2]int64
		next   string
	}
	fetch := func(t *testing.T, query string) page {
		t.Helper()
		r := ok(t, w.h, base+"/payments?"+query)
		if r.body["kind"] != "x402_payments" || r.body["chain"] != "eip155:143" {
			t.Fatalf("envelope %s", r.raw)
		}
		var p page
		for _, it := range itemsOf(t, r) {
			p.ids = append(p.ids, it["id"].(string))
			p.blocks = append(p.blocks, [2]int64{intOf(t, it["block"]), intOf(t, it["log_index"])})
		}
		p.next, _ = r.body["next_cursor"].(string)
		if _, present := r.body["next_cursor"]; present && p.next == "" {
			t.Errorf("next_cursor is not a string: %s", r.raw)
		}
		return p
	}
	t.Run("three_pages_without_duplicates_or_gaps", func(t *testing.T) {
		var all []string
		var blocks [][2]int64
		query, sizes := "payer="+pagingPayer+"&limit=50", []int{}
		for range 5 {
			p := fetch(t, query)
			sizes = append(sizes, len(p.ids))
			all, blocks = append(all, p.ids...), append(blocks, p.blocks...)
			if p.next == "" {
				break
			}
			query = "payer=" + pagingPayer + "&limit=50&cursor=" + p.next
		}
		if !slices.Equal(sizes, []int{50, 50, 20}) {
			t.Fatalf("page sizes %v", sizes)
		}
		if len(all) != 120 || len(slices.Compact(slices.Sorted(slices.Values(all)))) != 120 {
			t.Fatalf("%d ids, duplicates or gaps", len(all))
		}
		for i := 1; i < len(blocks); i++ {
			if blocks[i-1][0] < blocks[i][0] || (blocks[i-1][0] == blocks[i][0] && blocks[i-1][1] <= blocks[i][1]) {
				t.Fatalf("not newest first at %d: %v then %v", i, blocks[i-1], blocks[i])
			}
		}
		if all[0] != "143_5039_3" || all[119] != "143_5000_1" {
			t.Errorf("ends %s .. %s", all[0], all[119])
		}
	})
	t.Run("a_day_window_pages_newest_first", func(t *testing.T) {
		query, day := "limit=50", dayString(w.day-11)
		var sizes []int
		var last [2]int64
		for range 5 {
			p := fetch(t, "from="+day+"&to="+day+"&"+query)
			sizes = append(sizes, len(p.ids))
			if len(sizes) == 1 && (p.ids[0] != "143_5039_3" || p.next != "5023.2") {
				t.Fatalf("first page %s .. cursor %q", p.ids[0], p.next)
			}
			last = p.blocks[len(p.blocks)-1]
			if p.next == "" {
				break
			}
			query = "limit=50&cursor=" + p.next
		}
		if !slices.Equal(sizes, []int{50, 50, 20}) || last != [2]int64{5000, 1} {
			t.Errorf("page sizes %v, last row %v", sizes, last)
		}
	})
	t.Run("the_cursor_is_block_dot_log_index", func(t *testing.T) {
		if p := fetch(t, "payer="+pagingPayer+"&limit=2"); p.next != "5039.2" {
			t.Errorf("next_cursor %q", p.next)
		}
	})
	t.Run("an_exact_fit_has_no_next_cursor", func(t *testing.T) {
		if p := fetch(t, "payer="+pagingPayer+"&limit=120"); p.next != "" || len(p.ids) != 120 {
			t.Errorf("%d ids, cursor %q", len(p.ids), p.next)
		}
	})
	t.Run("uppercase_payer", func(t *testing.T) {
		if p := fetch(t, "payer="+upperHex(pagingPayer)+"&limit=200"); len(p.ids) != 120 {
			t.Errorf("%d ids", len(p.ids))
		}
	})
	t.Run("filters", func(t *testing.T) {
		cases := []struct {
			query string
			n     int
			key   string
			value any
		}{
			{"method=other&tier=all", 1, "method", "other"},
			{"method=other", 0, "", nil},
			{"method=eip3009_transfer&payee=" + strayPayee, 2, "payee", strayPayee},
			{"facilitator=monad-foundation&limit=200", 200, "facilitator", "monad-foundation"},
			{"facilitator=" + unlisted + "&tier=AB", 1, "relayer", unlisted},
			{"facilitator=unknown&tier=AB", 1, "tier", "B"},
			{"relayer=" + testRelayer + "&tier=all", 1, "tier", "X"},
			{"relayer=" + upperHex(molandak) + "&limit=5", 5, "relayer", molandak},
			{"payer=" + testkit.Addr(1034), 0, "", nil},
			{"payer=" + testkit.Addr(1034) + "&tier=all", 1, "reason", "test_relayer"},
			{"payer=" + testkit.Addr(1030), 0, "", nil},
			{"tier=AB&payer=" + testkit.Addr(1030), 1, "tier", "B"},
			{"payer=" + ausdPayer + "&from=2026-02-03&to=2026-02-03", 1, "block", 4000.0},
			{"payer=" + ausdPayer + "&from=2026-02-04", 0, "", nil},
			{"payer=" + ausdPayer + "&to=2026-02-02", 0, "", nil},
			{"payee=" + strayPayee + "&from=" + dayString(w.day-3) + "&to=" + dayString(w.day-3), 1, "block", 3200.0},
			{"payee=" + strayPayee + "&from=" + dayString(w.day-6) + "&to=" + dayString(w.day-6), 1, "block", 3100.0},
			{"payee=" + strayPayee + "&from=" + dayString(w.day-5) + "&to=" + dayString(w.day-4), 0, "", nil},
			{"payee=" + strayPayee + "&to=2020-01-01", 0, "", nil},
			{"payee=" + strayPayee + "&from=2100-01-01", 0, "", nil},
			{"relayer=" + molandak + "&from=" + dayString(w.day-1) + "&to=" + dayString(w.day-1) + "&limit=5", 5, "relayer", molandak},
			{"method=other&tier=all&from=" + dayString(w.day-8) + "&to=" + dayString(w.day-8), 1, "method", "other"},
			{"method=other&tier=all&from=" + dayString(w.day-7) + "&to=" + dayString(w.day), 0, "", nil},
		}
		for _, c := range cases {
			t.Run(c.query, func(t *testing.T) {
				items := itemsOf(t, ok(t, w.h, base+"/payments?"+c.query))
				if len(items) != c.n {
					t.Fatalf("%d items, want %d", len(items), c.n)
				}
				for _, it := range items {
					if it[c.key] != c.value {
						t.Fatalf("%s = %v, want %v", c.key, it[c.key], c.value)
					}
				}
			})
		}
	})
	t.Run("the_default_tier_excludes_X_and_all_includes_it", func(t *testing.T) {
		count := func(q string) int {
			n := 0
			for page := ""; ; {
				p := fetch(t, "limit=200"+q+page)
				n += len(p.ids)
				if p.next == "" {
					return n
				}
				page = "&cursor=" + p.next
			}
		}
		var tierA, tierAB, all int64
		err := w.d.Store.Pool.QueryRow(t.Context(), `select count(*) filter (where tier = 'A'), count(*) filter (where tier in ('A', 'B')), count(*) from payment`).Scan(&tierA, &tierAB, &all)
		if err != nil {
			t.Fatal(err)
		}
		if got := count(""); int64(got) != tierA {
			t.Errorf("default tier gave %d rows, tier A has %d", got, tierA)
		}
		if got := count("&tier=AB"); int64(got) != tierAB {
			t.Errorf("tier AB gave %d rows, want %d", got, tierAB)
		}
		if got := count("&tier=all"); int64(got) != all || int64(w.served) != all {
			t.Errorf("tier all gave %d rows, payment holds %d, seeded %d", got, all, w.served)
		}
	})
}

func TestTx(t *testing.T) {
	w := build(t)
	t.Run("both_rows_of_the_two_row_tx_in_log_order", func(t *testing.T) {
		r := ok(t, w.h, base+"/payments/"+twoTx)
		if r.body["kind"] != "x402_tx" || r.body["chain"] != "eip155:143" || r.body["tx"] != twoTx {
			t.Fatalf("envelope %s", r.raw)
		}
		items := itemsOf(t, r)
		if len(items) != 2 || items[0]["log_index"] != 1.0 || items[1]["log_index"] != 2.0 || items[1]["amount"] != "6" {
			t.Fatalf("items %v", items)
		}
	})
	t.Run("an_uppercase_hash_is_accepted", func(t *testing.T) {
		r := ok(t, w.h, base+"/payments/0x"+strings.ToUpper(twoTx[2:]))
		if len(itemsOf(t, r)) != 2 || r.body["tx"] != twoTx {
			t.Errorf("%s", r.raw)
		}
	})
	t.Run("a_tier_X_row_is_served_too", func(t *testing.T) {
		if p := firstItem(t, ok(t, w.h, base+"/payments/"+settleTx)); p["tier"] != "X" {
			t.Errorf("row %v", p)
		}
	})
	t.Run("a_hash_newer_than_the_watermark_is_404", func(t *testing.T) {
		wantProblem(t, get(t, w.h, base+"/payments/"+lateTx), http.StatusNotFound, "not_found")
		if got := scalar[int](t, w.d, `select count(*) from {src}."X402Payment" where "txHash" = $1`, lateTx); got != 1 {
			t.Fatalf("the late row is not in the source (%d)", got)
		}
	})
	t.Run("an_unknown_hash_is_404", func(t *testing.T) {
		wantProblem(t, get(t, w.h, base+"/payments/"+unknownTx), http.StatusNotFound, "not_found")
	})
}

func TestRowsBeyondTheWatermarkAreNeverRead(t *testing.T) {
	w := build(t)
	const landed = `insert into payment select chain_id, block + 1000, log_index, block_time, day, decode(repeat('ab', 32), 'hex'), method, token, payer, payee, amount, nonce,
		valid_after, valid_before, relayer, tx_to, selector, gas_cost, tier, facilitator_id, reason from payment where block = $1 and log_index = 1`
	if _, err := w.d.Store.Pool.Exec(t.Context(), landed, int64(5039)); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, w.d, `select count(*) from payment where block > $1`, int64(cutoff)); n != 1 {
		t.Fatalf("%d rows past the watermark", n)
	}
	wantProblem(t, get(t, w.h, base+"/payments/0x"+strings.Repeat("ab", 32)), http.StatusNotFound, "not_found")
	for _, p := range []string{"/payments?tier=all&limit=1", "/payments?payer=" + pagingPayer + "&limit=1"} {
		for _, it := range itemsOf(t, ok(t, w.h, base+p)) {
			if it["block"].(float64) > cutoff {
				t.Errorf("%s served block %v past the watermark %d", p, it["block"], cutoff)
			}
		}
	}
}

func TestProblems(t *testing.T) {
	h := seeded(t)
	cases := []struct {
		name, path string
		status     int
		code       string
	}{
		{"unknown parameter", base + "/leaderboards/facilitators?foo=1", 400, "invalid_query"},
		{"unknown parameter on payments", base + "/payments?foo=1", 400, "invalid_query"},
		{"unknown parameter on status", base + "/status?foo=1", 400, "invalid_query"},
		{"unknown parameter on registry", base + "/registry?foo=1", 400, "invalid_query"},
		{"unknown parameter on daily", base + "/sellers/" + burstSeller + "/daily?limit=5", 400, "invalid_query"},
		{"parameter given twice", base + "/payments?limit=1&limit=2", 400, "invalid_query"},
		{"empty parameter", base + "/payments?limit=", 400, "invalid_query"},
		{"malformed query string", base + "/payments?limit=%zz", 400, "invalid_query"},
		{"from after to", base + "/leaderboards/facilitators?from=2026-02-02&to=2026-02-01", 400, "invalid_query"},
		{"from after to on daily", base + "/sellers/" + burstSeller + "/daily?from=2026-02-02&to=2026-02-01", 400, "invalid_query"},
		{"367 day span", base + "/leaderboards/facilitators?from=2025-01-01&to=2026-01-02", 400, "invalid_query"},
		{"367 day span on daily", base + "/facilitators/monad-foundation/daily?from=2025-01-01&to=2026-01-02", 400, "invalid_query"},
		{"limit zero", base + "/leaderboards/facilitators?limit=0", 400, "invalid_query"},
		{"limit not a number", base + "/leaderboards/facilitators?limit=x", 400, "invalid_query"},
		{"leaderboard limit 101", base + "/leaderboards/facilitators?limit=101", 400, "invalid_query"},
		{"payments limit zero", base + "/payments?limit=0", 400, "invalid_query"},
		{"payments limit 201", base + "/payments?limit=201", 400, "invalid_query"},
		{"tier Z", base + "/leaderboards/facilitators?tier=Z", 400, "invalid_query"},
		{"tier all on a leaderboard", base + "/leaderboards/facilitators?tier=all", 400, "invalid_query"},
		{"payments tier Z", base + "/payments?tier=Z", 400, "invalid_query"},
		{"order x", base + "/leaderboards/facilitators?order=x", 400, "invalid_query"},
		{"cursor abc", base + "/payments?cursor=abc", 400, "invalid_query"},
		{"cursor with a third part", base + "/payments?cursor=1.2.3", 400, "invalid_query"},
		{"cursor negative", base + "/payments?cursor=-1.2", 400, "invalid_query"},
		{"unknown method", base + "/payments?method=teleport", 400, "invalid_query"},
		{"day that does not exist", base + "/leaderboards/facilitators?from=2026-02-30", 400, "invalid_query"},
		{"day without zero padding", base + "/leaderboards/facilitators?from=2026-2-3", 400, "invalid_query"},
		{"payments tx too short", base + "/payments/0x12", 400, "invalid_id"},
		{"payments tx without prefix", base + "/payments/" + unknownTx[2:], 400, "invalid_id"},
		{"payer too short", base + "/payments?payer=0x12", 400, "invalid_id"},
		{"payee not hex", base + "/payments?payee=0x" + strings.Repeat("z", 40), 400, "invalid_id"},
		{"relayer too short", base + "/payments?relayer=0x12", 400, "invalid_id"},
		{"facilitator that is nothing", base + "/payments?facilitator=nope", 400, "invalid_id"},
		{"kind bogus on leaderboards", base + "/leaderboards/bogus", 400, "invalid_id"},
		{"kind bogus on daily", base + "/bogus/" + burstSeller + "/daily", 400, "invalid_id"},
		{"seller id too short", base + "/sellers/0x12/daily", 400, "invalid_id"},
		{"facilitator id that is nothing", base + "/facilitators/nope/daily", 400, "invalid_id"},
		{"chain foo", "/internal/v1/chains/foo/x402/status", 400, "invalid_id"},
		{"chain with an uppercase namespace", "/internal/v1/chains/EIP155:143/x402/status", 400, "invalid_id"},
		{"chain with an empty reference", "/internal/v1/chains/eip155:/x402/status", 400, "invalid_id"},
		{"another chain", "/internal/v1/chains/eip155:1/x402/status", 404, "unsupported_chain"},
		{"another chain on payments", "/internal/v1/chains/eip155:1/x402/payments", 404, "unsupported_chain"},
		{"another namespace", "/internal/v1/chains/solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp/x402/registry", 404, "unsupported_chain"},
		{"unrouted path", "/nope", 404, "not_found"},
		{"unrouted path under x402", base + "/nope", 404, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := get(t, h, c.path)
			wantProblem(t, r, c.status, c.code)
			if r.header.Get("X-Request-Id") == "" {
				t.Error("no X-Request-Id")
			}
		})
	}
	t.Run("the_catch_all_names_the_missing_route", func(t *testing.T) {
		r := get(t, h, "/nope")
		if r.body["detail"] != "No such route." || r.body["instance"] != "/nope" {
			t.Errorf("body %s", r.raw)
		}
	})
	t.Run("only_GET", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base+"/status", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST = %d", rec.Code)
		}
	})
}

func TestHeaders(t *testing.T) {
	w := build(t)
	dataRoutes := []string{"/leaderboards/facilitators", "/facilitators/monad-foundation/daily", "/payments", "/payments/" + twoTx}
	for _, p := range dataRoutes {
		t.Run("data_route_"+p, func(t *testing.T) {
			r := ok(t, w.h, base+p)
			if r.header.Get("Sona-Finality") != "finalized" || r.header.Get("Sona-Block") != strconv.Itoa(cutoff) ||
				r.header.Get("Sona-Block-Time") != strconv.FormatInt(w.progTime, 10) || r.header.Get("Content-Type") != "application/json" {
				t.Errorf("headers %v", r.header)
			}
			if id := r.header.Get("X-Request-Id"); len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
				t.Errorf("X-Request-Id %q", id)
			}
			if r.header.Get("Content-Length") != strconv.Itoa(len(r.raw)) {
				t.Errorf("Content-Length %q for %d bytes", r.header.Get("Content-Length"), len(r.raw))
			}
		})
	}
	t.Run("the_trace_id_of_a_traceparent_is_the_request_id", func(t *testing.T) {
		r := get(t, w.h, base+"/status", "traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		if r.header.Get("X-Request-Id") != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("X-Request-Id %q", r.header.Get("X-Request-Id"))
		}
	})
	t.Run("a_bad_traceparent_gets_a_random_id", func(t *testing.T) {
		for _, tp := range []string{"junk", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7"} {
			id := get(t, w.h, base+"/status", "traceparent", tp).header.Get("X-Request-Id")
			if len(id) != 32 || id == "4bf92f3577b34da6a3ce929d0e0e4736" || id == strings.Repeat("0", 32) {
				t.Errorf("traceparent %q gave %q", tp, id)
			}
		}
		a := get(t, w.h, base+"/status").header.Get("X-Request-Id")
		b := get(t, w.h, base+"/status").header.Get("X-Request-Id")
		if a == b {
			t.Errorf("two requests share the id %s", a)
		}
	})
	t.Run("the_probes_send_neither", func(t *testing.T) {
		for _, p := range []string{"/healthz", "/readyz"} {
			r := get(t, w.h, p)
			if r.code != http.StatusOK || r.header.Get("X-Request-Id") != "" || r.header.Get("Sona-Block") != "" || r.header.Get("Sona-Finality") != "" {
				t.Errorf("%s: %d %v", p, r.code, r.header)
			}
		}
	})
	t.Run("an_error_carries_the_request_id_but_no_stamp", func(t *testing.T) {
		r := get(t, w.h, base+"/payments?limit=0")
		if r.header.Get("X-Request-Id") == "" || r.header.Get("Sona-Block") != "" || r.header.Get("Sona-Finality") != "" {
			t.Errorf("headers %v", r.header)
		}
	})
}

func closedPoolHandler(t *testing.T, w *world) http.Handler {
	t.Helper()
	st, err := store.Connect(t.Context(), os.Getenv("X402_TEST_DSN"), w.d.Engine)
	if err != nil {
		t.Fatal(err)
	}
	h := handlerFor(t, st, w.reg, []config.Config{w.cfg}, NewMetrics(), func() bool { return true })
	if r := get(t, h, base+"/payments?limit=1"); r.code != http.StatusOK {
		t.Fatalf("the second pool does not work: %d %s", r.code, r.raw)
	}
	st.Pool.Close()
	return h
}

func TestDeadline(t *testing.T) {
	w := build(t)
	closed := closedPoolHandler(t, w)
	for _, v := range []string{"0", "-1", "-99999999999999999"} {
		t.Run("deadline_"+v+"_answers_504_without_a_database_read", func(t *testing.T) {
			for _, p := range []string{"/payments", "/leaderboards/facilitators", "/status", "/registry", "/payments/" + twoTx} {
				wantProblem(t, get(t, closed, base+p, "Sona-Deadline-Ms", v), http.StatusGatewayTimeout, "deadline_exceeded")
			}
		})
	}
	t.Run("a_number_that_is_not_one_falls_back_to_5000", func(t *testing.T) {
		for _, v := range []string{"abc", "", "1.5"} {
			if r := get(t, w.h, base+"/payments?limit=1", "Sona-Deadline-Ms", v); r.code != http.StatusOK {
				t.Errorf("Sona-Deadline-Ms %q gave %d", v, r.code)
			}
		}
	})
	t.Run("a_deadline_that_passes_during_the_read_answers_504", func(t *testing.T) {
		tx, err := w.d.Store.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if _, err := tx.Exec(t.Context(), `lock table payment in access exclusive mode`); err != nil {
			t.Fatal(err)
		}
		var logged bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
		defer slog.SetDefault(old)
		start := time.Now()
		r := get(t, w.h, base+"/payments", "Sona-Deadline-Ms", "200")
		wantProblem(t, r, http.StatusGatewayTimeout, "deadline_exceeded")
		if took := time.Since(start); took < 150*time.Millisecond || took > 2*time.Second {
			t.Errorf("answered after %s", took)
		}
		if id := r.header.Get("X-Request-Id"); !strings.Contains(logged.String(), "request_id="+id) {
			t.Errorf("the 504 is not logged with its request id %q: %s", id, logged.String())
		}
	})
	t.Run("a_huge_budget_is_accepted", func(t *testing.T) {
		if r := get(t, w.h, base+"/payments?limit=1", "Sona-Deadline-Ms", "9223372036854775807"); r.code != http.StatusOK {
			t.Errorf("gave %d", r.code)
		}
	})
}

func TestDatabaseDown(t *testing.T) {
	w := build(t)
	h := closedPoolHandler(t, w)
	for _, p := range []string{"/leaderboards/facilitators", "/facilitators/monad-foundation/daily", "/payments", "/payments/" + twoTx} {
		t.Run(p, func(t *testing.T) {
			r := get(t, h, base+p)
			wantProblem(t, r, http.StatusServiceUnavailable, "upstream_unavailable")
			if r.header.Get("Retry-After") != "1" {
				t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
			}
		})
	}
	t.Run("the_registry_does_not_need_the_database", func(t *testing.T) {
		if r := get(t, h, base+"/registry"); r.code != http.StatusOK {
			t.Errorf("registry = %d", r.code)
		}
	})
}

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func TestAStatementTheDatabaseRejectsIs500(t *testing.T) {
	w := build(t)
	if _, err := w.d.Store.Pool.Exec(t.Context(), `drop view payment_signals`); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(old)
	r := get(t, w.h, base+"/payments")
	wantProblem(t, r, http.StatusInternalServerError, "internal")
	if strings.Contains(string(r.raw), "payment_signals") {
		t.Errorf("the cause leaks into the body: %s", r.raw)
	}
	if id := r.header.Get("X-Request-Id"); !strings.Contains(logged.String(), "request_id="+id) || !strings.Contains(logged.String(), "payment_signals") {
		t.Errorf("the 5xx is not logged with its request id %q: %s", id, logged.String())
	}
}

func TestMetricsCountRequestsByRoutePattern(t *testing.T) {
	d, cfg, reg := load(t)
	m := NewMetrics()
	h := handlerFor(t, d.Store, reg, []config.Config{cfg}, m, func() bool { return true })
	get(t, h, base+"/registry")
	get(t, h, base+"/payments/"+unknownTx)
	get(t, h, "/nope")
	get(t, h, "/healthz")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`x402_request_duration_seconds_count{code="200",route="GET /internal/v1/chains/{chain}/x402/registry"} 1`,
		`x402_request_duration_seconds_count{code="503",route="GET /internal/v1/chains/{chain}/x402/payments/{tx}"} 1`,
		`x402_request_duration_seconds_count{code="404",route="unmatched"} 1`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(body, "healthz") {
		t.Error("the probes are counted")
	}
}

func TestRoutesAreTheSpecRoutes(t *testing.T) {
	var got []string
	for _, r := range routes {
		got = append(got, r.pattern)
	}
	want := []string{
		"GET /healthz", "GET /readyz",
		"GET /internal/v1/chains/{chain}/x402/status",
		"GET /internal/v1/chains/{chain}/x402/registry",
		"GET /internal/v1/chains/{chain}/x402/leaderboards/{kind}",
		"GET /internal/v1/chains/{chain}/x402/flows",
		"GET /internal/v1/chains/{chain}/x402/rules/daily",
		"GET /internal/v1/chains/{chain}/x402/{kind}/{id}/daily",
		"GET /internal/v1/chains/{chain}/x402/payments",
		"GET /internal/v1/chains/{chain}/x402/payments/{tx}",
	}
	if !slices.Equal(got, want) {
		t.Errorf("routes\n got %q\nwant %q", got, want)
	}
}

func TestRequestID(t *testing.T) {
	const trace = "4bf92f3577b34da6a3ce929d0e0e4736"
	cases := []struct {
		name, header string
		fromHeader   bool
	}{
		{"valid", "00-" + trace + "-00f067aa0ba902b7-01", true},
		{"unsampled", "00-" + trace + "-00f067aa0ba902b7-00", true},
		{"empty", "", false},
		{"short", "00-4bf92f35-00f067aa0ba902b7-01", false},
		{"zero trace", "00-" + strings.Repeat("0", 32) + "-00f067aa0ba902b7-01", false},
		{"uppercase", "00-" + strings.ToUpper(trace) + "-00f067aa0ba902b7-01", false},
		{"not hex", "00-" + strings.Repeat("g", 32) + "-00f067aa0ba902b7-01", false},
		{"no dashes", "00" + trace + "00f067aa0ba902b7" + "01" + "xxx", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := requestID(c.header)
			if len(id) != 32 || (id == trace) != c.fromHeader {
				t.Errorf("requestID(%q) = %q", c.header, id)
			}
		})
	}
}
