package server

import (
	"strconv"
	"strings"
	"testing"
)

func TestDecimalValue(t *testing.T) {
	for raw, want := range map[string]string{"0": "0", "1": "0.000001", "41086200": "41.0862", "1000000": "1", "123456789": "123.456789", "10": "0.00001"} {
		if got := decimalValue(raw, 6); got != want {
			t.Errorf("decimalValue(%s) = %s, want %s", raw, got, want)
		}
	}
}

func TestFlows(t *testing.T) {
	w := build(t)
	var payments, volume, genuine string
	err := w.d.Store.Pool.QueryRow(t.Context(), `
		select sum(raw_payments)::text, sum(raw_volume)::text, sum(genuine_payments)::text from daily_flow
		where chain_id = 143 and facilitator_id = 'monad-foundation' and payee = decode(substr($1::text, 3), 'hex')`, burstSeller).Scan(&payments, &volume, &genuine)
	if err != nil || payments != "1518" {
		t.Fatalf("seed flow %s %s, err %v", payments, volume, err)
	}
	t.Run("default_window_and_order", func(t *testing.T) {
		r := ok(t, w.h, base+"/flows?tier=AB")
		win, _ := r.body["window"].(map[string]any)
		if r.body["kind"] != "x402_flows" || win["from"] != dayString(w.day-29) || win["to"] != dayString(w.day) {
			t.Fatalf("envelope %s", r.raw)
		}
		if len(itemsOf(t, r)) < 2 {
			t.Fatalf("both tiers should show %s", r.raw)
		}
		var top map[string]any
		for _, it := range itemsOf(t, ok(t, w.h, base+"/flows?order=raw_volume&limit=100")) {
			if it["payee"] == burstSeller {
				top = it
			}
		}
		vol, _ := top["raw_volume"].(map[string]any)
		gv, _ := top["genuine_volume"].(map[string]any)
		if top["facilitator"] != "monad-foundation" || top["name"] != "Molandak" || top["tier"] != "A" || intOf(t, top["raw_payments"]) != 1518 ||
			vol["raw"] != volume || vol["decimals"] != float64(6) || gv["decimals"] != float64(6) || strconv.FormatInt(intOf(t, top["genuine_payments"]), 10) != genuine {
			t.Fatalf("top %v", top)
		}
		if _, has := top["days_active"]; has || r.header.Get("Sona-Finality") != "finalized" {
			t.Errorf("item %v headers %v", top, r.header)
		}
	})
	for _, order := range []string{"genuine_volume", "raw_volume", "genuine_payments", "raw_payments"} {
		t.Run("order_"+order, func(t *testing.T) {
			items := itemsOf(t, ok(t, w.h, base+"/flows?tier=AB&order="+order))
			key := func(it map[string]any) float64 {
				if strings.HasSuffix(order, "volume") {
					f, _ := strconv.ParseFloat(it[order].(map[string]any)["raw"].(string), 64)
					return f
				}
				return float64(intOf(t, it[order]))
			}
			for i := 1; i < len(items); i++ {
				if key(items[i-1]) < key(items[i]) {
					t.Fatalf("%s not descending at %d", order, i)
				}
			}
		})
	}
	t.Run("tier_A_only", func(t *testing.T) {
		for _, it := range itemsOf(t, ok(t, w.h, base+"/flows?tier=A&limit=2")) {
			if it["tier"] != "A" {
				t.Fatalf("item %v", it)
			}
		}
	})
	t.Run("unlisted_relayer_has_no_name", func(t *testing.T) {
		for _, it := range itemsOf(t, ok(t, w.h, base+"/flows?tier=AB")) {
			if it["tier"] == "B" && (it["name"] != nil || it["facilitator"] == "monad-foundation") {
				t.Fatalf("item %v", it)
			}
		}
	})
	t.Run("window_outside_the_data", func(t *testing.T) {
		if items := itemsOf(t, ok(t, w.h, base+"/flows?from=2020-01-01&to=2020-01-31")); len(items) != 0 {
			t.Fatalf("items %v", items)
		}
	})
	t.Run("a_90_day_span_is_allowed", func(t *testing.T) {
		ok(t, w.h, base+"/flows?from=2026-01-01&to=2026-03-31")
	})
	for _, c := range []struct{ name, path string }{
		{"bad date", "/flows?from=2026-13-01"},
		{"from after to", "/flows?from=2026-02-02&to=2026-02-01"},
		{"span over 366 days", "/flows?from=2025-01-01&to=2026-01-02"},
		{"span over 90 days", "/flows?from=2026-01-01&to=2026-04-01"},
		{"tier X", "/flows?tier=X"},
		{"order count", "/flows?order=count"},
		{"limit zero", "/flows?limit=0"},
		{"limit 101", "/flows?limit=101"},
		{"unknown key", "/flows?kind=x"},
		{"rules unknown key", "/rules/daily?tier=A"},
		{"rules bad date", "/rules/daily?to=yesterday"},
	} {
		t.Run(c.name, func(t *testing.T) { wantProblem(t, get(t, w.h, base+c.path), 400, "invalid_query") })
	}
}

func TestRulesDaily(t *testing.T) {
	w := build(t)
	r := ok(t, w.h, base+"/rules/daily")
	win, _ := r.body["window"].(map[string]any)
	if r.body["kind"] != "x402_rules_daily" || win["from"] != dayString(w.day-29) || win["to"] != dayString(w.day) {
		t.Fatalf("envelope %s", r.raw)
	}
	items := itemsOf(t, r)
	var fixed, self int64
	prev := ""
	for _, it := range items {
		key := it["day"].(string) + " " + it["rule"].(string)
		if key <= prev {
			t.Errorf("%s comes after %s", key, prev)
		}
		prev = key
		switch it["rule"] {
		case "fixed_amount":
			fixed += intOf(t, it["count"])
		case "self_payment":
			self += intOf(t, it["count"])
		}
	}
	for _, it := range items {
		if intOf(t, it["payments"]) < intOf(t, it["count"]) {
			t.Fatalf("item %v fires for more payments than were checked", it)
		}
	}
	wantFixed := scalar[int64](t, w.d, `select count(*) from payment_signals where chain_id = 143 and fixed_amount`)
	if fixed != wantFixed || fixed == 0 || self != 1 {
		t.Fatalf("fixed_amount %d (want %d), self_payment %d", fixed, wantFixed, self)
	}
	if items := itemsOf(t, ok(t, w.h, base+"/rules/daily?from=2020-01-01&to=2020-01-31")); len(items) != 0 {
		t.Fatalf("items %v", items)
	}
}

func TestBaseFlowsAndRulesAnswerForBase(t *testing.T) {
	w := buildTwoChains(t)
	r := ok(t, w.h, baseRoot+"/flows")
	top := firstItem(t, r)
	if top["facilitator"] != "coinbase-cdp" || top["name"] != "Coinbase CDP" || intOf(t, top["raw_payments"]) != 2 || top["raw_volume"].(map[string]any)["raw"] != "7500" {
		t.Fatalf("top %v", top)
	}
	if r := ok(t, w.h, baseRoot+"/rules/daily"); r.body["kind"] != "x402_rules_daily" {
		t.Fatalf("rules %s", r.raw)
	}
	if items := itemsOf(t, ok(t, w.h, base+"/flows")); len(items) != 1 || items[0]["facilitator"] == "coinbase-cdp" {
		t.Fatalf("Monad flows %v", items)
	}
}
