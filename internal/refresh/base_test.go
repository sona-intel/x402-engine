package refresh

import (
	"context"
	"errors"
	"testing"
	"time"

	"x402/internal/config"
	"x402/internal/registry"
	"x402/internal/testkit"
)

const (
	baseUSDC   = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	cdpSigner  = "0x14fda13953fc30428938e6bf950d036e77214e52"
	cdpCodes   = "w=cdp_facil1"
	baseChain  = "eip155:8453"
	retention  = 90
	secondsDay = 86400
)

type both struct {
	monad, base *testkit.DB
	mcfg, bcfg  config.Config
	reg         *registry.Registry
}

func setupBoth(t *testing.T, progress int64) both {
	t.Helper()
	d, mcfg, reg := setup(t, progress)
	b := d.Other(t, 8453, true)
	b.SetProgress(t, progress, indexerTime)
	cfgs, err := config.Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	bcfg := cfgs[1]
	bcfg.SourceSchema = b.Source
	return both{d, b, mcfg, bcfg, reg}
}

func (w both) onceBase(t *testing.T) Result {
	t.Helper()
	r, ok := w.reg.For(baseChain)
	if !ok {
		t.Fatal("no Base registry")
	}
	res, err := Once(t.Context(), w.monad.Store, w.bcfg, r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func basePay(block int64, relayer, codes string) testkit.Row {
	r := pay(block)
	r.Token, r.TxTo, r.Relayer, r.Codes = baseUSDC, baseUSDC, relayer, codes
	return r
}

func TestBaseRowsLandUnderTheirOwnChain(t *testing.T) {
	w := setupBoth(t, 1000)
	w.monad.Insert(t, pay(10), pay(11))
	w.base.Insert(t, basePay(10, cdpSigner, ""), basePay(12, cdpSigner, ""), basePay(13, cdpSigner, ""))
	once(t, w.monad, w.mcfg, w.reg)
	if res := w.onceBase(t); res.Ingested != 3 {
		t.Fatalf("Base ingested %d, want 3", res.Ingested)
	}
	for chain, want := range map[int]int{143: 2, 8453: 3} {
		if n := scalar[int](t, w.monad, `select count(*) from payment where chain_id = $1`, chain); n != want {
			t.Errorf("chain %d holds %d payments, want %d", chain, n, want)
		}
	}
	for chain, want := range map[int]int64{143: 2, 8453: 3} {
		m, err := w.monad.Store.Meta(t.Context(), chain)
		if err != nil || m.RowsPayment != want || m.WatermarkBlock != 990 {
			t.Errorf("chain %d meta %+v, err %v", chain, m, err)
		}
	}
}

func TestBaseVolumeCountsOnlyBaseUSDC(t *testing.T) {
	w := setupBoth(t, 1000)
	monadUSDC := basePay(10, cdpSigner, "")
	monadUSDC.Token = testkit.USDC
	w.base.Insert(t, basePay(11, cdpSigner, ""), monadUSDC)
	w.onceBase(t)
	vol := scalar[string](t, w.monad, `select coalesce(sum(raw_volume), 0)::text from daily_facilitator where chain_id = 8453`)
	cnt := scalar[int](t, w.monad, `select coalesce(sum(raw_payments), 0)::int from daily_facilitator where chain_id = 8453`)
	if vol != "1000000" || cnt != 2 {
		t.Fatalf("volume %s over %d payments, want 1000000 over 2", vol, cnt)
	}
}

func TestBuilderCodeMakesTheCDPFacilitator(t *testing.T) {
	w := setupBoth(t, 1000)
	other := "0x2222222222222222222222222222222222222222"
	notX402 := basePay(13, cdpSigner, cdpCodes)
	notX402.Method = "other"
	w.base.Insert(t,
		basePay(10, cdpSigner, cdpCodes),
		basePay(11, other, ""),
		basePay(12, other, "bc_tyhmcm97"),
		notX402,
		basePay(14, other, "cdp_facil1"),
		basePay(15, cdpSigner, ""),
		basePay(16, cdpSigner, "w=xcdp_facil1"),
		basePay(17, cdpSigner, "w=cdp_facil10"),
		basePay(18, cdpSigner, "a=b,w=cdp_facil1"),
		basePay(19, cdpSigner, "bc_tyhmcm97|cdp_facil1"),
	)
	w.onceBase(t)
	type got struct{ tier, facilitator string }
	want := map[int64]got{
		10: {"A", "coinbase-cdp"}, 11: {"B", other}, 12: {"B", other}, 13: {"X", ""}, 14: {"B", other}, 15: {"B", cdpSigner},
		16: {"B", cdpSigner}, 17: {"B", cdpSigner}, 18: {"A", "coinbase-cdp"}, 19: {"A", "coinbase-cdp"},
	}
	for block, g := range want {
		var tier string
		var facilitator *string
		if err := w.monad.Store.Pool.QueryRow(t.Context(), `select tier, facilitator_id from payment where chain_id = 8453 and block = $1`, block).Scan(&tier, &facilitator); err != nil {
			t.Fatal(err)
		}
		f := ""
		if facilitator != nil {
			f = *facilitator
		}
		if tier != g.tier || f != g.facilitator {
			t.Errorf("block %d got %s %q, want %s %q", block, tier, f, g.tier, g.facilitator)
		}
	}
	if m, _ := w.monad.Store.Meta(t.Context(), 8453); m.RowsTierA != 3 || m.RowsTierB != 6 || m.RowsTierX != 1 {
		t.Fatalf("tier counts %+v", m)
	}
}

func TestBuilderCodeIsIgnoredWhereTheSourceHasNone(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	r := pay(10)
	r.Relayer = cdpSigner
	d.Insert(t, r)
	once(t, d, cfg, reg)
	if tier := scalar[string](t, d, `select tier from payment where chain_id = 143`); tier != "B" {
		t.Fatalf("tier %s, want B", tier)
	}
}

func TestChangingTheBaseRegistryRebuildsOnlyBase(t *testing.T) {
	w := setupBoth(t, 1000)
	w.monad.Insert(t, pay(10), pay(11))
	w.base.Insert(t, basePay(10, cdpSigner, cdpCodes), basePay(11, cdpSigner, ""))
	once(t, w.monad, w.mcfg, w.reg)
	w.onceBase(t)
	mbefore := meta(t, w.monad)

	edited := registryFrom(t, "builder_codes: [cdp_facil1]", "builder_codes: [cdp_facil2]", "version: 2026-10-13.2", "version: 2026-10-13.3")
	r, _ := edited.For(baseChain)
	res, err := Once(t.Context(), w.monad.Store, w.bcfg, r)
	if err != nil || res.Reset || res.Ingested != 2 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if tier := scalar[string](t, w.monad, `select tier from payment where chain_id = 8453 and block = 10`); tier != "B" {
		t.Fatalf("the old code still names the facilitator, tier %s", tier)
	}
	if res := once(t, w.monad, w.mcfg, edited); res.Ingested != 0 || res.Reset {
		t.Fatalf("Monad round after the Base edit %+v", res)
	}
	if mafter := meta(t, w.monad); mafter.RegistryChecksum != mbefore.RegistryChecksum || mafter.RowsPayment != 2 {
		t.Fatalf("Monad meta moved: %+v to %+v", mbefore, mafter)
	}
	if n := scalar[int](t, w.monad, `select count(*) from facilitator_code where chain_id = 8453 and code = 'cdp_facil2'`); n != 1 {
		t.Fatalf("%d codes stored", n)
	}
}

func TestAuditKeepsOneRowPerChainAndVersion(t *testing.T) {
	w := setupBoth(t, 1000)
	once(t, w.monad, w.mcfg, w.reg)
	w.onceBase(t)
	bump := registryFrom(t, "version: 2026-10-13.2", "version: 2026-10-12.1")
	r, _ := bump.For(baseChain)
	if _, err := Once(t.Context(), w.monad.Store, w.bcfg, r); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, w.monad, `select count(*) from registry_version where version = '2026-10-12.1'`); n != 2 {
		t.Fatalf("%d audit rows for the shared version, want 2", n)
	}
}

func TestEachChainTakesItsOwnLock(t *testing.T) {
	w := setupBoth(t, 1000)
	hold(t, w.monad, w.monad.Engine+":eip155:143")
	w.onceBase(t)
	if _, err := Once(t.Context(), w.monad.Store, w.mcfg, w.reg); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func pruneAt(day int64) time.Time { return time.Unix(day*secondsDay+3600, 0) }

func TestPruneDeletesWholeDaysPastTheRetention(t *testing.T) {
	w := setupBoth(t, 1000)
	today := int64(20000)
	old, edge, recent := basePay(10, cdpSigner, cdpCodes), basePay(11, cdpSigner, cdpCodes), basePay(12, cdpSigner, cdpCodes)
	old.BlockTime = (today - retention - 3) * secondsDay
	edge.BlockTime = (today-retention)*secondsDay + 5
	recent.BlockTime = (today - 5) * secondsDay
	ahead := basePay(995, cdpSigner, cdpCodes)
	ahead.BlockTime = old.BlockTime
	w.base.Insert(t, old, edge, recent, ahead)
	w.monad.Insert(t, func() testkit.Row { r := pay(10); r.BlockTime = old.BlockTime; return r }())
	once(t, w.monad, w.mcfg, w.reg)
	w.onceBase(t)
	days := scalar[int](t, w.monad, `select count(*) from daily_facilitator where chain_id = 8453`)

	res, err := Prune(t.Context(), w.monad.Store, w.bcfg, pruneAt(today))
	if err != nil {
		t.Fatal(err)
	}
	if res.Payments != 1 || res.Source != 1 {
		t.Fatalf("pruned %+v, want 1 engine row and 1 source row", res)
	}
	if blocks := column[int64](t, w.monad, `select block from payment where chain_id = 8453 order by block`); len(blocks) != 2 || blocks[0] != 11 || blocks[1] != 12 {
		t.Fatalf("Base blocks left %v", blocks)
	}
	if blocks := column[int64](t, w.base, `select "blockNumber"::bigint from {src}."X402Payment" order by 1`); len(blocks) != 3 || blocks[0] != 11 || blocks[1] != 12 || blocks[2] != 995 {
		t.Fatalf("source blocks left %v", blocks)
	}
	if got := scalar[int](t, w.monad, `select count(*) from daily_facilitator where chain_id = 8453`); got != days {
		t.Fatalf("daily rows went from %d to %d", days, got)
	}
	if m, _ := w.monad.Store.Meta(t.Context(), 8453); m.RowsPayment != 2 || m.RowsTierA != 2 {
		t.Fatalf("meta counts %+v", m)
	}
	if n := scalar[int](t, w.monad, `select count(*) from payment where chain_id = 143`); n != 1 {
		t.Fatalf("Monad holds %d payments after the Base prune, want 1", n)
	}
	if m := meta(t, w.monad); m.RowsPayment != 1 {
		t.Fatalf("Monad meta %+v", m)
	}
}

func TestAWipeKeepsTheDailyRowsOfPrunedDays(t *testing.T) {
	w := setupBoth(t, 1000)
	today := int64(20000)
	old, recent := basePay(10, cdpSigner, cdpCodes), basePay(11, cdpSigner, cdpCodes)
	old.BlockTime = (today - retention - 3) * secondsDay
	recent.BlockTime = (today - 5) * secondsDay
	w.base.Insert(t, old, recent)
	w.onceBase(t)
	oldDay := old.BlockTime / secondsDay
	if _, err := w.monad.Store.Pool.Exec(t.Context(), `insert into daily_rules (chain_id, day, rule, payments) values (8453, $1, 'fixed_amount', 7)`, oldDay); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(t.Context(), w.monad.Store, w.bcfg, pruneAt(today)); err != nil {
		t.Fatal(err)
	}
	edited := registryFrom(t, "version: 2026-10-13.2", "version: 2026-10-13.3")
	r, _ := edited.For(baseChain)
	if _, err := Once(t.Context(), w.monad.Store, w.bcfg, r); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"daily_facilitator", "daily_flow", "daily_rules"} {
		if n := scalar[int](t, w.monad, `select count(*) from `+table+` where chain_id = 8453 and day = $1`, oldDay); n == 0 {
			t.Errorf("%s lost the pruned day", table)
		}
	}
	if n := scalar[int](t, w.monad, `select count(*) from daily_facilitator where chain_id = 8453 and day = $1`, recent.BlockTime/secondsDay); n == 0 {
		t.Error("the day that still has payment rows was not rebuilt")
	}
}

func TestPruneWaitsForTheWatermarkToCatchUp(t *testing.T) {
	w := setupBoth(t, 1000)
	today := int64(20000)
	old := basePay(10, cdpSigner, cdpCodes)
	old.BlockTime = (today - retention - 3) * secondsDay
	w.base.Insert(t, old)
	w.onceBase(t)
	if _, err := w.monad.Store.Pool.Exec(t.Context(), `update meta set watermark_block = cutoff_block - 1 where chain_id = 8453`); err != nil {
		t.Fatal(err)
	}
	if res, err := Prune(t.Context(), w.monad.Store, w.bcfg, pruneAt(today)); err != nil || res != (Pruned{}) {
		t.Fatalf("pruned %+v, err %v", res, err)
	}
	if n := scalar[int](t, w.monad, `select count(*) from payment where chain_id = 8453`); n != 1 {
		t.Fatalf("%d payments left, want 1", n)
	}
}

func TestPruneDropsTheFlagRowsOfPrunedDays(t *testing.T) {
	w := setupBoth(t, 5000)
	today := int64(20000)
	oldTime, newTime := (today-retention-2)*secondsDay, (today-1)*secondsDay
	var rows []testkit.Row
	for _, at := range []struct {
		first int64
		time  int64
	}{{100, oldTime}, {300, newTime}} {
		for i := range 120 {
			r := basePay(at.first+int64(i), cdpSigner, "")
			r.BlockTime = at.time
			rows = append(rows, r)
		}
	}
	w.base.Insert(t, rows...)
	w.base.SetProgress(t, 5000, newTime)
	w.onceBase(t)
	if n := scalar[int](t, w.monad, `select count(*) from payee_day_flag where chain_id = 8453`); n != 2 {
		t.Fatalf("%d payee day flags before the prune, want 2", n)
	}
	if _, err := Prune(t.Context(), w.monad.Store, w.bcfg, pruneAt(today)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"payee_day_flag", "relayer_day_flag"} {
		if days := column[int](t, w.monad, `select day::int from `+table+` where chain_id = 8453`); len(days) != 1 || int64(days[0]) != newTime/secondsDay {
			t.Errorf("%s days left %v", table, days)
		}
	}
}

func TestPruneDoesNothingWithoutARetention(t *testing.T) {
	w := setupBoth(t, 1000)
	old := pay(10)
	old.BlockTime = 1000
	w.monad.Insert(t, old)
	once(t, w.monad, w.mcfg, w.reg)
	res, err := Prune(t.Context(), w.monad.Store, w.mcfg, time.Now())
	if err != nil || res != (Pruned{}) {
		t.Fatalf("pruned %+v, err %v", res, err)
	}
	if n := scalar[int](t, w.monad, `select count(*) from payment`); n != 1 {
		t.Fatalf("%d payments left", n)
	}
}

func TestPruneSkipsWhileARefreshHoldsTheChain(t *testing.T) {
	w := setupBoth(t, 1000)
	hold(t, w.monad, w.monad.Engine+":"+baseChain)
	if _, err := Prune(t.Context(), w.monad.Store, w.bcfg, time.Now()); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestPruneBeforeAnyIngestLeavesTheSourceAlone(t *testing.T) {
	w := setupBoth(t, 1000)
	old := basePay(10, cdpSigner, "")
	old.BlockTime = 1000
	w.base.Insert(t, old)
	if res, err := Prune(t.Context(), w.monad.Store, w.bcfg, time.Now()); err != nil || res != (Pruned{}) {
		t.Fatalf("pruned %+v, err %v", res, err)
	}
	if n := scalar[int](t, w.base, `select count(*) from {src}."X402Payment"`); n != 1 {
		t.Fatalf("%d source rows left", n)
	}
}

func TestPruneLoopRunsAtOnceAndEveryInterval(t *testing.T) {
	w := setupBoth(t, 1000)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runs := 0
	PruneLoop(ctx, w.monad.Store, w.bcfg, 5*time.Millisecond, func(_ Pruned, err error) {
		if err != nil {
			t.Error(err)
		}
		if runs++; runs == 3 {
			cancel()
		}
	})
	if runs != 3 {
		t.Fatalf("done called %d times, want 3", runs)
	}
}

func TestPruneLoopStaysQuietWhileTheChainIsHeld(t *testing.T) {
	w := setupBoth(t, 1000)
	hold(t, w.monad, w.monad.Engine+":"+baseChain)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	PruneLoop(ctx, w.monad.Store, w.bcfg, 5*time.Millisecond, func(Pruned, error) { t.Error("done called for a skipped run") })
}

func TestABigBatchRefreshesThePlannerStatistics(t *testing.T) {
	old := analyzeRows
	analyzeRows = 3
	t.Cleanup(func() { analyzeRows = old })
	w := setupBoth(t, 1000)
	w.base.Insert(t, basePay(10, cdpSigner, ""), basePay(11, cdpSigner, ""), basePay(12, cdpSigner, ""))
	w.onceBase(t)
	if n := scalar[int](t, w.monad, `select count(*) from pg_stats where schemaname = $1 and tablename = 'payment' and attname = 'chain_id'`, w.monad.Engine); n != 1 {
		t.Fatalf("%d statistics rows for payment.chain_id, want 1", n)
	}
}

func TestASmallBatchLeavesTheStatisticsAlone(t *testing.T) {
	w := setupBoth(t, 1000)
	w.base.Insert(t, basePay(10, cdpSigner, ""))
	w.onceBase(t)
	if n := scalar[int](t, w.monad, `select count(*) from pg_stats where schemaname = $1 and tablename = 'payment'`, w.monad.Engine); n != 0 {
		t.Fatalf("%d statistics rows after a small batch, want 0", n)
	}
}
