package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	KindFacilitators = "facilitators"
	KindSellers      = "sellers"
	KindPayers       = "payers"
)

var methods = [...]string{
	1: "eip3009_transfer", 2: "eip3009_receive", 3: "permit2_exact", 4: "permit2_upto", 5: "batch_deposit", methodOther: "other", 7: "eip3009_routed",
}

const methodOther = 6

func MethodName(ordinal int16) string {
	if ordinal < 1 || int(ordinal) >= len(methods) {
		return methods[methodOther]
	}
	return methods[ordinal]
}

func MethodOrdinal(name string) (int16, bool) {
	for i := 1; i < len(methods); i++ {
		if methods[i] == name {
			return int16(i), true
		}
	}
	return 0, false
}

func ValidKind(kind string) bool {
	_, ok := entities[kind]
	return ok
}

type entity struct {
	table, key string
	// id turns the grouped key column k into the served id, match turns the id parameter into a key value
	id, match   string
	gas, gasOut string
	dailyCols   string
}

var entities = map[string]entity{
	KindFacilitators: {
		table: "daily_facilitator", key: "facilitator_id", id: "k", match: "$2",
		gas: "sum(gas_cost) as gas_cost,", gasOut: "gas_cost::text", dailyCols: "raw_payers, raw_sellers, gas_cost::text",
	},
	KindSellers: {
		table: "daily_seller", key: "seller", id: "'0x' || encode(k, 'hex')", match: "decode(substr($2, 3), 'hex')",
		gasOut: "null::text", dailyCols: "raw_payers, null::bigint, null::text",
	},
	KindPayers: {
		table: "daily_payer", key: "payer", id: "'0x' || encode(k, 'hex')", match: "decode(substr($2, 3), 'hex')",
		gasOut: "null::text", dailyCols: "null::bigint, raw_sellers, null::text",
	},
}

var orders = []string{"genuine_volume", "raw_volume", "genuine_payments", "raw_payments"}

var (
	leaderQueries = buildLeaderQueries()
	dailyQueries  = buildDailyQueries()
)

func buildLeaderQueries() map[string]string {
	out := make(map[string]string, len(entities)*len(orders))
	for kind, e := range entities {
		for _, order := range orders {
			out[kind+"/"+order] = `
select ` + e.id + `, tier, raw_payments, raw_volume::text, genuine_payments, genuine_volume::text, ` + e.gasOut + `, active_days, first_day, last_day
from (
	select ` + e.key + ` as k, tier, sum(raw_payments)::bigint as raw_payments, sum(raw_volume) as raw_volume,
		sum(genuine_payments)::bigint as genuine_payments, sum(genuine_volume) as genuine_volume, ` + e.gas + `
		count(*) as active_days, min(day) as first_day, max(day) as last_day
	from ` + e.table + `
	where chain_id = $1 and day between $2 and $3 and tier = any($4::char(1)[])
	group by ` + e.key + `, tier order by ` + order + ` desc, ` + e.key + `, tier limit $5
) t order by t.` + order + ` desc, t.k, t.tier`
		}
	}
	return out
}

func buildDailyQueries() map[string]string {
	out := make(map[string]string, len(entities)*2)
	for kind, e := range entities {
		out[kind+"/rows"] = `
select day, tier, raw_payments, raw_volume::text, genuine_payments, genuine_volume::text, ` + e.dailyCols + `
from ` + e.table + `
where chain_id = $1 and ` + e.key + ` = ` + e.match + ` and day between $3 and $4 and tier = any($5::char(1)[])
order by day, tier`
		out[kind+"/known"] = `select exists (select 1 from ` + e.table + ` where chain_id = $1 and ` + e.key + ` = ` + e.match + `)`
	}
	return out
}

type LeaderQuery struct {
	Kind     string
	From, To int
	Tiers    []string
	Order    string
	Limit    int
}

type LeaderRow struct {
	ID              string
	Tier            string
	RawPayments     int64
	RawVolume       string
	GenuinePayments int64
	GenuineVolume   string
	GasCost         *string
	ActiveDays      int64
	FirstDay        int
	LastDay         int
}

func (s *Store) Leaderboard(ctx context.Context, chain int, q LeaderQuery) ([]LeaderRow, error) {
	sql, ok := leaderQueries[q.Kind+"/"+q.Order]
	if !ok {
		return nil, errors.New("leaderboard: unknown kind or order")
	}
	rows, err := s.Pool.Query(ctx, sql, chain, q.From, q.To, q.Tiers, q.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LeaderRow])
}

type DailyRow struct {
	Day             int
	Tier            string
	RawPayments     int64
	RawVolume       string
	GenuinePayments int64
	GenuineVolume   string
	RawPayers       *int64
	RawSellers      *int64
	GasCost         *string
}

func (s *Store) Daily(ctx context.Context, chain int, kind, id string, tiers []string, from, to int) (rows []DailyRow, known bool, err error) {
	sql, ok := dailyQueries[kind+"/rows"]
	if !ok {
		return nil, false, errors.New("daily: unknown kind")
	}
	r, err := s.Pool.Query(ctx, sql, chain, id, from, to, tiers)
	if err != nil {
		return nil, false, err
	}
	if rows, err = pgx.CollectRows(r, pgx.RowToStructByPos[DailyRow]); err != nil || len(rows) > 0 {
		return rows, len(rows) > 0, err
	}
	err = s.Pool.QueryRow(ctx, dailyQueries[kind+"/known"], chain, id).Scan(&known)
	return rows, known, err
}

func (s *Store) LastDay(ctx context.Context, chain int) (int, error) {
	var day int
	err := s.Pool.QueryRow(ctx, `select coalesce(max(day), 0) from daily_facilitator where chain_id = $1`, chain).Scan(&day)
	return day, err
}

type Cursor struct {
	Block    int64
	LogIndex int32
}

type PaymentFilter struct {
	UpTo        int64
	Payer       []byte
	Payee       []byte
	Relayer     []byte
	Facilitator string
	Method      int16
	// Tiers is every tier when empty
	Tiers   []string
	FromDay *int
	ToDay   *int
	After   *Cursor
	Limit   int
}

type PaymentRow struct {
	Block       int64
	LogIndex    int32
	BlockTime   int64
	Tx          []byte
	Method      int16
	Token       []byte
	Payer       []byte
	Payee       []byte
	Amount      *string
	Nonce       []byte
	ValidAfter  *string
	ValidBefore *string
	Relayer     []byte
	TxTo        []byte
	Selector    []byte
	GasCost     *string
	Tier        string
	Facilitator *string
	Reason      *string
	Genuine     *bool
	RulesFired  []string
}

const selectPayments = `
select block, log_index, block_time, tx, method, token, payer, payee, amount::text, nonce, valid_after::text, valid_before::text, relayer, tx_to,
	selector, gas_cost::text, tier, facilitator_id, reason, genuine,
	array_remove(array[case when fixed_amount then 'fixed_amount' end, case when payee_concentration then 'payee_concentration' end,
		case when no_other_activity then 'no_other_activity' end, case when bridge_funded then 'bridge_funded' end,
		case when self_payment then 'self_payment' end], null)
from payment_signals`

const paymentsByTxSQL = selectPayments + ` where chain_id = $1 and tx = $2 and block <= $3 order by block, log_index`

func (s *Store) Payments(ctx context.Context, chain int, f PaymentFilter) ([]PaymentRow, error) {
	args := []any{chain, f.UpTo}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	var q strings.Builder
	q.WriteString(selectPayments + ` where chain_id = $1 and block <= $2`)
	if f.Payer != nil {
		q.WriteString(" and payer = " + arg(f.Payer))
	}
	if f.Payee != nil {
		q.WriteString(" and payee = " + arg(f.Payee))
	}
	if f.Relayer != nil {
		q.WriteString(" and relayer = " + arg(f.Relayer))
	}
	if f.Facilitator != "" {
		q.WriteString(" and facilitator_id = " + arg(f.Facilitator))
	}
	if f.Method != 0 {
		q.WriteString(" and method = " + arg(f.Method))
	}
	if len(f.Tiers) > 0 {
		q.WriteString(" and tier = any(" + arg(f.Tiers) + "::char(1)[])")
	}
	// block times never go back, so the window's first and last block bound it for every block-ordered index
	if f.FromDay != nil {
		from := arg(*f.FromDay)
		q.WriteString(" and day >= " + from + " and block >= (select block from payment where chain_id = $1 and day >= " + from + " order by day, block limit 1)")
	}
	if f.ToDay != nil {
		to := arg(*f.ToDay)
		q.WriteString(" and day <= " + to + " and block <= (select block from payment where chain_id = $1 and day <= " + to + " order by day desc, block desc limit 1)")
	}
	if f.After != nil {
		q.WriteString(" and (block, log_index) < (" + arg(f.After.Block) + ", " + arg(f.After.LogIndex) + ")")
	}
	order := " order by block desc, log_index desc"
	if (f.FromDay != nil || f.ToDay != nil) && f.Payer == nil && f.Payee == nil && f.Relayer == nil {
		// block times never go back, so a leading day lets the day index serve a window the block index would scan past
		order = " order by day desc, block desc, log_index desc"
	}
	// one row more than asked tells the caller whether a next page exists
	q.WriteString(order + " limit " + arg(f.Limit+1))
	rows, err := s.Pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PaymentRow])
}

func (s *Store) PaymentsByTx(ctx context.Context, chain int, tx []byte, upToBlock int64) ([]PaymentRow, error) {
	rows, err := s.Pool.Query(ctx, paymentsByTxSQL, chain, tx, upToBlock)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PaymentRow])
}

type FlowQuery struct {
	From, To int
	Tiers    []string
	Order    string
	Limit    int
}

type FlowRow struct {
	Facilitator     string
	Payee           string
	Tier            string
	RawPayments     int64
	RawVolume       string
	GenuinePayments int64
	GenuineVolume   string
}

func flowSQL(order string) string {
	return `
select facilitator_id, '0x' || encode(payee, 'hex'), tier, sum(raw_payments)::bigint, sum(raw_volume)::text,
	sum(genuine_payments)::bigint, sum(genuine_volume)::text
from daily_flow
where chain_id = $1 and day between $2 and $3 and tier = any($4::char(1)[])
group by facilitator_id, payee, tier
order by sum(` + order + `) desc, facilitator_id, payee, tier limit $5`
}

var flowQueries = func() map[string]string {
	out := make(map[string]string, len(orders))
	for _, o := range orders {
		out[o] = flowSQL(o)
	}
	return out
}()

func (s *Store) Flows(ctx context.Context, chain int, q FlowQuery) ([]FlowRow, error) {
	sql, ok := flowQueries[q.Order]
	if !ok {
		return nil, errors.New("flows: unknown order")
	}
	rows, err := s.Pool.Query(ctx, sql, chain, q.From, q.To, q.Tiers, q.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[FlowRow])
}

type RuleDayRow struct {
	Day      int
	Rule     string
	Count    int64
	Payments int64
}

func (s *Store) RulesDaily(ctx context.Context, chain, from, to int) ([]RuleDayRow, error) {
	rows, err := s.Pool.Query(ctx, `
select r.day, r.rule, r.payments, t.total
from daily_rules r
join (select day, sum(raw_payments)::bigint as total from daily_facilitator where chain_id = $1 and day between $2 and $3 group by day) t on t.day = r.day
where r.chain_id = $1 and r.day between $2 and $3 order by r.day, r.rule`, chain, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RuleDayRow])
}
