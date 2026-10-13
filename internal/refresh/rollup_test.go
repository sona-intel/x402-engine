package refresh

import (
	"testing"

	"x402/internal/testkit"
)

type ruleDay struct {
	day      int
	rule     string
	payments int64
}

func ruleDays(t *testing.T, d *testkit.DB, chain int) []ruleDay {
	t.Helper()
	rows, err := d.Store.Pool.Query(t.Context(), d.SQL(`select day, rule, payments from daily_rules where chain_id = $1 order by day, rule`), chain)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []ruleDay
	for rows.Next() {
		var r ruleDay
		if err := rows.Scan(&r.day, &r.rule, &r.payments); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestFlowAndRuleRollupsCountWhatTheRowsShow(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	rows := testkit.Burst(10, 120, testkit.BurstOpts{Payers: 3, Payee: testkit.Addr(20), Relayer: molandak, Amount: "1000000", BlockTime: indexerTime})
	self := pay(200)
	self.Payer, self.Payee = testkit.Addr(9), testkit.Addr(9)
	other := pay(201)
	other.Token, other.Payer, other.Payee = "0x000000000000000000000000000000000000dead", testkit.Addr(30), testkit.Addr(5)
	d.Insert(t, append(rows, self, other)...)
	once(t, d, cfg, reg)

	type flow struct {
		payee    string
		payments int64
		volume   string
	}
	rowsOut, err := d.Store.Pool.Query(t.Context(), d.SQL(`select '0x' || encode(payee, 'hex'), raw_payments, raw_volume::text from daily_flow where chain_id = 143 order by payee`))
	if err != nil {
		t.Fatal(err)
	}
	defer rowsOut.Close()
	got := map[string]flow{}
	for rowsOut.Next() {
		var f flow
		if err := rowsOut.Scan(&f.payee, &f.payments, &f.volume); err != nil {
			t.Fatal(err)
		}
		got[f.payee] = f
	}
	want := map[string]flow{
		testkit.Addr(20): {testkit.Addr(20), 120, "120000000"},
		testkit.Addr(9):  {testkit.Addr(9), 1, "1000000"},
		testkit.Addr(5):  {testkit.Addr(5), 1, "0"},
	}
	if len(got) != len(want) {
		t.Fatalf("flows %+v, want %+v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("flow %s = %+v, want %+v", k, got[k], w)
		}
	}
	byRule := map[string]int64{}
	for _, r := range ruleDays(t, d, 143) {
		byRule[r.rule] = r.payments
	}
	if byRule["fixed_amount"] != 120 || byRule["payee_concentration"] != 120 || byRule["no_other_activity"] != 120 || byRule["self_payment"] != 1 || len(byRule) != 4 {
		t.Fatalf("rules %v", byRule)
	}
}

func TestRollupsAreRebuiltForChainsThatPredateThem(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, burst(10, 120)...)
	once(t, d, cfg, reg)
	before := ruleDays(t, d, 143)
	if _, err := d.Store.Pool.Exec(t.Context(), d.SQL(`truncate daily_flow, daily_rules`)); err != nil {
		t.Fatal(err)
	}
	once(t, d, cfg, reg)
	if n := scalar[int](t, d, `select count(*) from daily_flow where chain_id = 143`); n != 1 {
		t.Fatalf("%d flow rows after the refill, want 1", n)
	}
	if after := ruleDays(t, d, 143); len(after) != len(before) || len(after) == 0 {
		t.Fatalf("rules %v, want %v", after, before)
	}
}

func TestPrunedDaysKeepTheirRollupRows(t *testing.T) {
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
	flowsBefore := column[int](t, w.monad, `select day::int from daily_flow where chain_id = 8453 order by day`)
	rulesBefore := ruleDays(t, w.monad, 8453)
	if len(flowsBefore) != 2 || len(rulesBefore) == 0 {
		t.Fatalf("flow days %v, rule rows %v", flowsBefore, rulesBefore)
	}
	if _, err := Prune(t.Context(), w.monad.Store, w.bcfg, pruneAt(today)); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, w.monad, `select count(*) from payment where chain_id = 8453 and day = $1`, oldTime/secondsDay); n != 0 {
		t.Fatalf("%d payments left on the pruned day", n)
	}
	// a later payment from a payer of the pruned day makes that payer's days dirty, and the pruned day has no rows left to rebuild from
	late := basePay(600, cdpSigner, "")
	late.BlockTime = newTime + 60
	w.base.Insert(t, late)
	w.onceBase(t)
	if flows := column[int](t, w.monad, `select day::int from daily_flow where chain_id = 8453 order by day`); len(flows) != 2 || flows[0] != flowsBefore[0] {
		t.Fatalf("flow days %v, want the pruned day %d kept", flows, flowsBefore[0])
	}
	var kept int
	for _, r := range ruleDays(t, w.monad, 8453) {
		if int64(r.day) == oldTime/secondsDay {
			kept++
		}
	}
	var wasKept int
	for _, r := range rulesBefore {
		if int64(r.day) == oldTime/secondsDay {
			wasKept++
		}
	}
	if kept != wasKept || kept == 0 {
		t.Fatalf("%d rule rows on the pruned day, want %d", kept, wasKept)
	}
	if got := scalar[int](t, w.monad, `select raw_payments::int from daily_flow where chain_id = 8453 and day = $1`, oldTime/secondsDay); got != 120 {
		t.Fatalf("pruned day counts %d payments, want 120", got)
	}
}
