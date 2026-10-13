package server

import (
	"net/http"

	"x402/internal/store"
)

type counters struct {
	RawPayments     int64  `json:"raw_payments"`
	RawVolume       string `json:"raw_volume"`
	GenuinePayments int64  `json:"genuine_payments"`
	GenuineVolume   string `json:"genuine_volume"`
}

type span struct {
	ActiveDays int64  `json:"active_days"`
	FirstDay   string `json:"first_day"`
	LastDay    string `json:"last_day"`
}

type facilitatorItem struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
	Tier string  `json:"tier"`
	counters
	GasCost string `json:"gas_cost"`
	span
}

type entityItem struct {
	ID   string `json:"id"`
	Tier string `json:"tier"`
	counters
	span
}

type leaderboardBody struct {
	Kind  string `json:"kind"`
	Chain string `json:"chain"`
	Board string `json:"board"`
	From  string `json:"from"`
	To    string `json:"to"`
	Tier  string `json:"tier"`
	Order string `json:"order"`
	Items any    `json:"items"`
}

func (a *app) leaderboards() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		kind := c.req.PathValue("kind")
		if !store.ValidKind(kind) {
			return nil, invalidID("A leaderboard is facilitators, sellers or payers.")
		}
		q, err := parseQuery(c.req, "from", "to", "tier", "order", "limit")
		if err != nil {
			return nil, err
		}
		win, err := parseWindow(q)
		if err != nil {
			return nil, err
		}
		tier, tiers, err := parseTier(q, false)
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
		from, to, err := c.resolve(win)
		if err != nil {
			return nil, err
		}
		rows, err := a.st.Leaderboard(c.ctx, c.ch.id, store.LeaderQuery{Kind: kind, From: from, To: to, Tiers: tiers, Order: order, Limit: limit})
		if err != nil {
			return nil, err
		}
		b := leaderboardBody{Kind: "x402_leaderboard", Chain: c.ch.name, Board: kind, From: formatDay(from), To: formatDay(to), Tier: tier, Order: order}
		if kind == store.KindFacilitators {
			b.Items = c.ch.facilitatorItems(rows)
		} else {
			b.Items = entityItems(rows)
		}
		return marshal(b), nil
	})
}

func newCounters(r store.LeaderRow) counters {
	return counters{r.RawPayments, r.RawVolume, r.GenuinePayments, r.GenuineVolume}
}

func newSpan(r store.LeaderRow) span {
	return span{r.ActiveDays, formatDay(r.FirstDay), formatDay(r.LastDay)}
}

func (ch *chainApp) facilitatorItems(rows []store.LeaderRow) []facilitatorItem {
	items := make([]facilitatorItem, len(rows))
	for i, r := range rows {
		it := facilitatorItem{ID: r.ID, Tier: r.Tier, counters: newCounters(r), span: newSpan(r)}
		if r.GasCost != nil {
			it.GasCost = *r.GasCost
		}
		if name, ok := ch.facilitators[r.ID]; ok {
			it.Name = &name
		}
		items[i] = it
	}
	return items
}

func entityItems(rows []store.LeaderRow) []entityItem {
	items := make([]entityItem, len(rows))
	for i, r := range rows {
		items[i] = entityItem{ID: r.ID, Tier: r.Tier, counters: newCounters(r), span: newSpan(r)}
	}
	return items
}

type dayItem struct {
	Day  string `json:"day"`
	Tier string `json:"tier"`
	counters
	RawPayers  *int64  `json:"raw_payers,omitzero"`
	RawSellers *int64  `json:"raw_sellers,omitzero"`
	GasCost    *string `json:"gas_cost,omitzero"`
}

type dailyBody struct {
	Kind   string    `json:"kind"`
	Chain  string    `json:"chain"`
	Entity string    `json:"entity"`
	ID     string    `json:"id"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Tier   string    `json:"tier"`
	Items  []dayItem `json:"items"`
}

func (a *app) daily() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		kind := c.req.PathValue("kind")
		if !store.ValidKind(kind) {
			return nil, invalidID("An entity is facilitators, sellers or payers.")
		}
		var id string
		var err error
		if kind == store.KindFacilitators {
			id, err = c.ch.parseFacilitator(c.req.PathValue("id"))
		} else {
			id, err = parseEntity(c.req.PathValue("id"))
		}
		if err != nil {
			return nil, err
		}
		q, err := parseQuery(c.req, "from", "to", "tier")
		if err != nil {
			return nil, err
		}
		win, err := parseWindow(q)
		if err != nil {
			return nil, err
		}
		tier, tiers, err := parseTier(q, false)
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
		rows, known, err := a.st.Daily(c.ctx, c.ch.id, kind, id, tiers, from, to)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, missing("No payments were recorded for this " + kind[:len(kind)-1] + ".")
		}
		b := dailyBody{Kind: "x402_daily", Chain: c.ch.name, Entity: kind, ID: id, From: formatDay(from), To: formatDay(to), Tier: tier, Items: make([]dayItem, len(rows))}
		for i, r := range rows {
			b.Items[i] = dayItem{
				Day: formatDay(r.Day), Tier: r.Tier, counters: counters{r.RawPayments, r.RawVolume, r.GenuinePayments, r.GenuineVolume},
				RawPayers: r.RawPayers, RawSellers: r.RawSellers, GasCost: r.GasCost,
			}
		}
		return marshal(b), nil
	})
}
