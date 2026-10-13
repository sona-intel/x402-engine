package server

import (
	"net/http"
	"strings"

	"x402/internal/store"
)

// usdcDecimals is fixed because volume counts USDC only
const usdcDecimals = 6

type windowBody struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type amountBody struct {
	Raw      string `json:"raw"`
	Decimals int    `json:"decimals"`
	Value    string `json:"value"`
}

type flowItem struct {
	Facilitator     string     `json:"facilitator"`
	Name            *string    `json:"name"`
	Payee           string     `json:"payee"`
	Tier            string     `json:"tier"`
	RawPayments     int64      `json:"raw_payments"`
	RawVolume       amountBody `json:"raw_volume"`
	GenuinePayments int64      `json:"genuine_payments"`
	GenuineVolume   amountBody `json:"genuine_volume"`
}

type flowsBody struct {
	Kind   string     `json:"kind"`
	Window windowBody `json:"window"`
	Items  []flowItem `json:"items"`
}

func amount(raw string) amountBody {
	return amountBody{raw, usdcDecimals, decimalValue(raw, usdcDecimals)}
}

// decimalValue places the point of a base-unit integer string without a float, because an amount can have 78 digits
func decimalValue(raw string, decimals int) string {
	if len(raw) <= decimals {
		raw = strings.Repeat("0", decimals-len(raw)+1) + raw
	}
	whole, frac := raw[:len(raw)-decimals], strings.TrimRight(raw[len(raw)-decimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

func (a *app) flows() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		q, err := parseQuery(c.req, "from", "to", "tier", "order", "limit")
		if err != nil {
			return nil, err
		}
		win, err := parseWindow(q)
		if err != nil {
			return nil, err
		}
		_, tiers, err := parseTier(q, false)
		if err != nil {
			return nil, err
		}
		order, err := parseOrder(q)
		if err != nil {
			return nil, err
		}
		limit, err := parseLimit(q, 25, 100)
		if err != nil {
			return nil, err
		}
		if _, err := c.data(); err != nil {
			return nil, err
		}
		from, to, err := c.resolveUpTo(win, maxFlowDays)
		if err != nil {
			return nil, err
		}
		rows, err := a.st.Flows(c.ctx, c.ch.id, store.FlowQuery{From: from, To: to, Tiers: tiers, Order: order, Limit: limit})
		if err != nil {
			return nil, err
		}
		b := flowsBody{Kind: "x402_flows", Window: windowBody{formatDay(from), formatDay(to)}, Items: make([]flowItem, len(rows))}
		for i, r := range rows {
			it := flowItem{
				Facilitator: r.Facilitator, Payee: r.Payee, Tier: r.Tier, RawPayments: r.RawPayments, RawVolume: amount(r.RawVolume),
				GenuinePayments: r.GenuinePayments, GenuineVolume: amount(r.GenuineVolume),
			}
			if name, ok := c.ch.facilitators[r.Facilitator]; ok {
				it.Name = &name
			}
			b.Items[i] = it
		}
		return marshal(b), nil
	})
}

type ruleDayItem struct {
	Day      string `json:"day"`
	Rule     string `json:"rule"`
	Count    int64  `json:"count"`
	Payments int64  `json:"payments"`
}

type rulesDailyBody struct {
	Kind   string        `json:"kind"`
	Window windowBody    `json:"window"`
	Items  []ruleDayItem `json:"items"`
}

func (a *app) rulesDaily() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		q, err := parseQuery(c.req, "from", "to")
		if err != nil {
			return nil, err
		}
		win, err := parseWindow(q)
		if err != nil {
			return nil, err
		}
		if _, err := c.data(); err != nil {
			return nil, err
		}
		from, to, err := c.resolve(win)
		if err != nil {
			return nil, err
		}
		rows, err := a.st.RulesDaily(c.ctx, c.ch.id, from, to)
		if err != nil {
			return nil, err
		}
		b := rulesDailyBody{Kind: "x402_rules_daily", Window: windowBody{formatDay(from), formatDay(to)}, Items: make([]ruleDayItem, len(rows))}
		for i, r := range rows {
			b.Items[i] = ruleDayItem{formatDay(r.Day), r.Rule, r.Count, r.Payments}
		}
		return marshal(b), nil
	})
}
