package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"x402/internal/config"
	"x402/internal/refresh"
	"x402/internal/testkit"
)

const (
	baseRoot = "/internal/v1/chains/eip155:8453/x402"
	baseUSDC = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	cdpTx    = "0x14fda13953fc30428938e6bf950d036e77214e52"
	cdpCode  = "w=cdp_facil1"
)

type twoChains struct {
	h    http.Handler
	cfgs []config.Config
	day  int64
}

func buildTwoChains(t *testing.T) twoChains { return buildTwoChainsWith(t, nil) }

func buildTwoChainsWith(t *testing.T, extra func(day int64) []testkit.Row) twoChains {
	t.Helper()
	d, mcfg, reg := load(t)
	b := d.Other(t, 8453, true)
	cfgs, err := config.Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	mcfg.SourceSchema = d.Source
	bcfg := cfgs[1]
	bcfg.SourceSchema = b.Source
	progTime := time.Now().Unix() - 60
	day := progTime / 86400
	d.SetProgress(t, progress, progTime)
	d.Insert(t, usdc(testkit.Row{Block: 100, LogIndex: 1, BlockTime: dayTime(day, 50), Tx: testkit.Hash(100), Payer: testkit.Addr(1), Payee: testkit.Addr(2), Amount: "700"}))
	rows := []testkit.Row{
		{Block: 200, LogIndex: 1, BlockTime: dayTime(day, 60), Tx: testkit.Hash(200), Method: "eip3009_transfer", Token: baseUSDC, Payer: testkit.Addr(11), Payee: testkit.Addr(12), Nonce: testkit.Hash(2000), Relayer: cdpTx, TxTo: baseUSDC, Selector: "0xe3ee160e", Amount: "5000", Codes: cdpCode},
		{Block: 201, LogIndex: 4, BlockTime: dayTime(day, 61), Tx: testkit.Hash(201), Method: "eip3009_transfer", Token: baseUSDC, Payer: testkit.Addr(13), Payee: testkit.Addr(12), Nonce: testkit.Hash(2001), Relayer: cdpTx, TxTo: baseUSDC, Selector: "0xe3ee160e", Amount: "2500", Codes: cdpCode},
	}
	baseProgress := int64(progress)
	if extra != nil {
		rows = append(rows, extra(day)...)
		baseProgress = rows[len(rows)-1].Block + 100
	}
	b.SetProgress(t, baseProgress, progTime)
	b.Insert(t, rows...)
	for _, c := range []struct {
		cfg config.Config
	}{{mcfg}, {bcfg}} {
		r, _ := reg.For(c.cfg.Chain)
		if _, err := refresh.Once(t.Context(), d.Store, c.cfg, r); err != nil {
			t.Fatal(err)
		}
	}
	return twoChains{h: handlerFor(t, d.Store, reg, []config.Config{mcfg, bcfg}, NewMetrics(), func() bool { return true }), cfgs: []config.Config{mcfg, bcfg}, day: day}
}

func TestBaseStatusAnswersWithItsOwnRowsAndCoverage(t *testing.T) {
	w := buildTwoChains(t)
	r := get(t, w.h, baseRoot+"/status")
	if r.code != 200 || r.body["chain"] != "eip155:8453" || r.body["registry_version"] != "2026-10-16.1" {
		t.Fatalf("status %d %s", r.code, r.raw)
	}
	if rows := r.body["rows"].(map[string]any); rows["payments"] != float64(2) || rows["tier_a"] != float64(2) {
		t.Fatalf("rows %v", rows)
	}
	codes := []string{}
	for _, c := range r.body["coverage"].([]any) {
		codes = append(codes, c.(map[string]any)["code"].(string))
	}
	if got := strings.Join(codes, ","); !strings.Contains(got, "history_starts") || !strings.Contains(got, "payment_rows_pruned") {
		t.Fatalf("coverage codes %s", got)
	}
	if !strings.Contains(string(r.raw), "2026-09-13") || !strings.Contains(string(r.raw), "last 90 days") {
		t.Fatalf("coverage lacks the start date: %s", r.raw)
	}
	m := get(t, w.h, base+"/status")
	if rows := m.body["rows"].(map[string]any); rows["payments"] != float64(1) {
		t.Fatalf("Monad rows %v", rows)
	}
	if strings.Contains(string(m.raw), "history_starts") || strings.Contains(string(m.raw), "payment_rows_pruned") {
		t.Fatalf("Monad coverage changed: %s", m.raw)
	}
}

func TestBaseLeaderboardNamesTheCDPFacilitator(t *testing.T) {
	w := buildTwoChains(t)
	r := get(t, w.h, baseRoot+"/leaderboards/facilitators")
	items, _ := r.body["items"].([]any)
	if r.code != 200 || len(items) != 1 {
		t.Fatalf("leaderboard %d %s", r.code, r.raw)
	}
	it := items[0].(map[string]any)
	if it["id"] != "coinbase-cdp" || it["name"] != "Coinbase CDP" || it["raw_payments"] != float64(2) || it["raw_volume"] != "7500" {
		t.Fatalf("item %v", it)
	}
	if m := get(t, w.h, base+"/leaderboards/facilitators"); strings.Contains(string(m.raw), "coinbase-cdp") {
		t.Fatalf("a Base facilitator leaked into Monad: %s", m.raw)
	}
	if r := get(t, w.h, baseRoot+"/facilitators/coinbase-cdp/daily"); r.code != 200 {
		t.Fatalf("daily %d %s", r.code, r.raw)
	}
	if r := get(t, w.h, base+"/facilitators/coinbase-cdp/daily"); r.code != 400 {
		t.Fatalf("a Base facilitator on Monad answered %d", r.code)
	}
}

func TestBasePaymentsCarryTheBaseIDAndChain(t *testing.T) {
	w := buildTwoChains(t)
	r := get(t, w.h, baseRoot+"/payments")
	items, _ := r.body["items"].([]any)
	if r.code != 200 || len(items) != 2 {
		t.Fatalf("payments %d %s", r.code, r.raw)
	}
	first := items[0].(map[string]any)
	if first["id"] != "8453_201_4" || first["chain"] != "eip155:8453" || first["tier"] != "A" || first["facilitator"] != "coinbase-cdp" || first["token"] != baseUSDC {
		t.Fatalf("payment %v", first)
	}
	if tx := get(t, w.h, baseRoot+"/payments/"+testkit.Hash(200)); tx.code != 200 || tx.body["chain"] != "eip155:8453" {
		t.Fatalf("tx %d %s", tx.code, tx.raw)
	}
	if tx := get(t, w.h, base+"/payments/"+testkit.Hash(200)); tx.code != 404 {
		t.Fatalf("a Base transaction on Monad answered %d", tx.code)
	}
}

func TestBaseRegistryListsTheBuilderCode(t *testing.T) {
	w := buildTwoChains(t)
	r := get(t, w.h, baseRoot+"/registry")
	if r.code != 200 || r.body["version"] != "2026-10-16.1" {
		t.Fatalf("registry %d %s", r.code, r.raw)
	}
	fs := r.body["facilitators"].([]any)
	f := fs[0].(map[string]any)
	if len(fs) != 2 || f["id"] != "coinbase-cdp" || len(f["addresses"].([]any)) != 20 || f["builder_codes"].([]any)[0] != "cdp_facil1" {
		t.Fatalf("facilitators %v", fs)
	}
	if p := fs[1].(map[string]any); p["id"] != "payai" || len(p["addresses"].([]any)) != 4 || p["builder_codes"] != nil {
		t.Fatalf("PayAI %v", p)
	}
	if len(r.body["relayers"].([]any)) != 24 {
		t.Fatalf("relayers %v", r.body["relayers"])
	}
	m := get(t, w.h, base+"/registry")
	if m.body["checksum"] != "123a6e6c0d5498edb63b4af92319dd147a5b64b5956ec4e522089cd68f74ad19" || strings.Contains(string(m.raw), "builder_codes") {
		t.Fatalf("Monad registry changed: %s", m.raw)
	}
}

const (
	payaiSigner = "0xc6699d2aada6c36dfea5c248dd70f9cb0235cb63"
	multicall3  = "0xca11bde05977b3631167028862be2a173976ca11"
)

func routedRows(day int64) []testkit.Row {
	routed := func(block int64, relayer string, payer int) testkit.Row {
		return testkit.Row{
			Block: block, LogIndex: 2, BlockTime: dayTime(day, int64(40+payer)), Tx: testkit.Hash(int(block)), Method: "eip3009_routed", Token: baseUSDC,
			Payer: testkit.Addr(payer), Payee: testkit.Addr(12), Nonce: testkit.Hash(int(block) + 10000), Relayer: relayer, TxTo: multicall3,
			Selector: "0x82ad56cb", Amount: "10000", ValidAfter: "0", ValidBefore: "1791213857",
		}
	}
	return []testkit.Row{routed(52662884, payaiSigner, 21), routed(52662885, unlisted, 22)}
}

func routedHandler(t *testing.T) http.Handler { return buildTwoChainsWith(t, routedRows).h }

func TestRoutedPaymentsCountForAListedFacilitatorOnly(t *testing.T) {
	h := routedHandler(t)
	items := itemsOf(t, ok(t, h, baseRoot+"/payments?method=eip3009_routed&tier=all"))
	if len(items) != 2 {
		t.Fatalf("%d routed payments, want 2", len(items))
	}
	unlistedRow, payai := items[0], items[1]
	if payai["method"] != "eip3009_routed" || payai["tier"] != "A" || payai["facilitator"] != "payai" || payai["reason"] != nil ||
		payai["payee"] != testkit.Addr(12) || payai["amount"] != "10000" || payai["tx_to"] != multicall3 || payai["genuine"] == nil {
		t.Fatalf("PayAI payment %v", payai)
	}
	if unlistedRow["tier"] != "X" || unlistedRow["reason"] != "routed_unlisted" || unlistedRow["facilitator"] != nil || unlistedRow["genuine"] != nil {
		t.Fatalf("unlisted payment %v", unlistedRow)
	}
	if n := len(itemsOf(t, ok(t, h, baseRoot+"/payments?method=eip3009_routed"))); n != 1 {
		t.Fatalf("tier A holds %d routed payments, want 1", n)
	}
	ids := []string{}
	for _, it := range itemsOf(t, ok(t, h, baseRoot+"/leaderboards/facilitators")) {
		ids = append(ids, it["id"].(string))
	}
	if slices.Sort(ids); strings.Join(ids, ",") != "coinbase-cdp,payai" {
		t.Fatalf("leaderboard ids %v", ids)
	}
}

func TestUnsupportedChainNamesBothChains(t *testing.T) {
	w := buildTwoChains(t)
	r := get(t, w.h, "/internal/v1/chains/eip155:1/x402/status")
	wantProblem(t, r, 404, "unsupported_chain")
	if d := r.body["detail"]; d != "Only eip155:143, eip155:8453 are served." {
		t.Fatalf("detail %v", d)
	}
}

func TestNewRefusesAChainTheRegistryLacks(t *testing.T) {
	d, cfg, reg := load(t)
	cfg.Chain, cfg.ChainID = "eip155:10", 10
	if _, err := New(d.Store, reg, []config.Config{cfg}, NewMetrics(), func() bool { return true }); err == nil {
		t.Fatal("a chain without a registry was accepted")
	}
}

func TestOnlyAGatewayChainNamesGatewayBatches(t *testing.T) {
	arc := coverageFor(config.Config{GatewayWallet: "0x77777777dcc4d5a8b6e418fd04d8997ef11000ee"})
	if last := arc[len(arc)-1]; last.Code != "gateway_batches_not_counted" || !strings.Contains(last.Note, "0x77777777dcc4d5a8b6e418fd04d8997ef11000ee") {
		t.Fatalf("Arc coverage %v", arc)
	}
	for _, c := range coverageFor(config.Config{}) {
		if c.Code == "gateway_batches_not_counted" {
			t.Fatalf("Monad coverage names Gateway batches")
		}
	}
}
