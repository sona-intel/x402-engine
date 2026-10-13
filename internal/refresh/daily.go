package refresh

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const dayCounters = `count(*), coalesce(sum(amount) filter (where token = decode(substr($2::text, 3), 'hex')), 0), count(*) filter (where genuine),
	coalesce(sum(amount) filter (where token = decode(substr($2::text, 3), 'hex') and genuine), 0)`

const (
	dirtyDaysSQL = `
create temp table dirty_day on commit drop as
select day from payment where chain_id = $1 and block > $2 and block <= $3 and tier in ('A', 'B')
union
select p.day from payer_change c join payment p on p.chain_id = $1 and p.payer = c.p and p.tier in ('A', 'B')`
	// without statistics on the day list the planner scans all of payment for the dirty days
	analyzeDirtyDaysSQL = `analyze dirty_day`
	dayRowsSQL          = `
create temp table day_row on commit drop as
select day, tier, facilitator_id, payer, payee, token, amount, gas_cost, genuine,
	fixed_amount, payee_concentration, no_other_activity, bridge_funded, self_payment
from payment_signals where chain_id = $1 and tier in ('A', 'B') and day in (select day from dirty_day)`
	dropFacilitatorDaysSQL = `delete from daily_facilitator where chain_id = $1 and day in (select day from dirty_day)`
	dropSellerDaysSQL      = `delete from daily_seller where chain_id = $1 and day in (select day from dirty_day)`
	dropPayerDaysSQL       = `delete from daily_payer where chain_id = $1 and day in (select day from dirty_day)`
	dropFlowDaysSQL        = `delete from daily_flow where chain_id = $1 and day in (select day from dirty_day)`
	dropRuleDaysSQL        = `delete from daily_rules where chain_id = $1 and day in (select day from dirty_day)`
	addFacilitatorDaysSQL  = `
insert into daily_facilitator (chain_id, day, tier, facilitator_id, raw_payments, raw_volume, genuine_payments, genuine_volume, gas_cost, raw_sellers, raw_payers)
select $1, day, tier, facilitator_id, ` + dayCounters + `, coalesce(sum(gas_cost), 0), count(distinct payee), count(distinct payer)
from day_row group by day, tier, facilitator_id`
	addSellerDaysSQL = `
insert into daily_seller (chain_id, day, tier, seller, raw_payments, raw_volume, genuine_payments, genuine_volume, raw_payers)
select $1, day, tier, payee, ` + dayCounters + `, count(distinct payer)
from day_row where payee is not null group by day, tier, payee`
	addFlowDaysSQL = `
insert into daily_flow (chain_id, day, tier, facilitator_id, payee, raw_payments, raw_volume, genuine_payments, genuine_volume)
select $1, day, tier, facilitator_id, payee, ` + dayCounters + `
from day_row where payee is not null and facilitator_id is not null group by day, tier, facilitator_id, payee`
	addRuleDaysSQL = `
insert into daily_rules (chain_id, day, rule, payments)
select $1, day, f.rule, count(*)
from day_row cross join lateral (values ('fixed_amount', fixed_amount), ('payee_concentration', payee_concentration),
	('no_other_activity', no_other_activity), ('bridge_funded', bridge_funded), ('self_payment', self_payment)) f(rule, fired)
where f.fired group by day, f.rule`
	addPayerDaysSQL = `
insert into daily_payer (chain_id, day, tier, payer, raw_payments, raw_volume, genuine_payments, genuine_volume, raw_sellers)
select $1, day, tier, payer, ` + dayCounters + `, count(distinct payee)
from day_row where payer is not null group by day, tier, payer`
)

func rebuildDays(ctx context.Context, tx pgx.Tx, chain int, lo, hi int64, volumeToken string) (int, error) {
	tag, err := tx.Exec(ctx, dirtyDaysSQL, chain, lo, hi)
	if err != nil {
		return 0, err
	}
	err = run(ctx, tx,
		stmt{analyzeDirtyDaysSQL, nil},
		stmt{dayRowsSQL, []any{chain}},
		stmt{dropFacilitatorDaysSQL, []any{chain}},
		stmt{dropSellerDaysSQL, []any{chain}},
		stmt{dropPayerDaysSQL, []any{chain}},
		stmt{dropFlowDaysSQL, []any{chain}},
		stmt{dropRuleDaysSQL, []any{chain}},
		stmt{addFacilitatorDaysSQL, []any{chain, volumeToken}},
		stmt{addSellerDaysSQL, []any{chain, volumeToken}},
		stmt{addPayerDaysSQL, []any{chain, volumeToken}},
		stmt{addFlowDaysSQL, []any{chain, volumeToken}},
		stmt{addRuleDaysSQL, []any{chain}})
	return int(tag.RowsAffected()), err
}

const (
	allDaysSQL = `create temp table dirty_day on commit drop as select distinct day from payment where chain_id = $1 and tier in ('A', 'B')`
	// chains ingested before these two tables existed get one pass over the days their rows still cover
	needsRollupSQL = `
select exists (select 1 from payment where chain_id = $1 and tier in ('A', 'B'))
	and not exists (select 1 from daily_flow where chain_id = $1) and not exists (select 1 from daily_rules where chain_id = $1)`
)

func backfillRollups(ctx context.Context, conn *pgxpool.Conn, chain int, volumeToken string) error {
	var needed bool
	if err := conn.QueryRow(ctx, needsRollupSQL, chain).Scan(&needed); err != nil || !needed {
		return err
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		return run(ctx, tx,
			stmt{allDaysSQL, []any{chain}},
			stmt{analyzeDirtyDaysSQL, nil},
			stmt{dayRowsSQL, []any{chain}},
			stmt{addFlowDaysSQL, []any{chain, volumeToken}},
			stmt{addRuleDaysSQL, []any{chain}})
	})
}
