package refresh

import (
	"context"

	"github.com/jackc/pgx/v5"

	"x402/internal/config"
)

const (
	touchedPayeeDays   = `select distinct payee, day from payment where chain_id = $1 and block > $2 and block <= $3 and tier in ('A', 'B') and payee is not null`
	touchedRelayerDays = `select distinct relayer, day from payment where chain_id = $1 and block > $2 and block <= $3 and tier in ('A', 'B') and relayer is not null`
)

const (
	dropPayeeDayFlagsSQL = `
with t as (` + touchedPayeeDays + `)
delete from payee_day_flag f using t where f.chain_id = $1 and f.payee = t.payee and f.day = t.day`
	addPayeeDayFlagsSQL = `
with t as (` + touchedPayeeDays + `)
insert into payee_day_flag (chain_id, payee, day, fixed_amount)
select $1, payee, day, amount from (
	select p.payee, p.day, p.amount, count(*) as c, sum(count(*)) over (partition by p.payee, p.day) as n
	from payment p join t on t.payee = p.payee and t.day = p.day
	where p.chain_id = $1 and p.tier in ('A', 'B')
	group by p.payee, p.day, p.amount) g
where amount is not null and n >= $4::int and c >= $5::float8::numeric * n`
	dropRelayerDayFlagsSQL = `
with t as (` + touchedRelayerDays + `)
delete from relayer_day_flag f using t where f.chain_id = $1 and f.relayer = t.relayer and f.day = t.day`
	addRelayerDayFlagsSQL = `
with t as (` + touchedRelayerDays + `)
insert into relayer_day_flag (chain_id, relayer, day, payee)
select $1, relayer, day, payee from (
	select p.relayer, p.day, p.payee, count(*) as c, sum(count(*)) over (partition by p.relayer, p.day) as n
	from payment p join t on t.relayer = p.relayer and t.day = p.day
	where p.chain_id = $1 and p.tier in ('A', 'B')
	group by p.relayer, p.day, p.payee) g
where payee is not null and n >= $4::int and c >= $5::float8::numeric * n`
)

const (
	// the relayers come from a skip scan because a per-payer probe on so few distinct values plans as a full table scan
	payerChangeSQL = `
create temp table payer_change on commit drop as
with recursive relayers(r) as (
	(select relayer from payment where chain_id = $1 and relayer is not null order by relayer limit 1)
	union all
	select (select relayer from payment where chain_id = $1 and relayer > relayers.r order by relayer limit 1) from relayers where relayers.r is not null
), named as (
	select distinct v.k as p
	from payment p cross join lateral (values (case when p.tier in ('A', 'B') then p.payer end), (p.payee), (p.relayer)) as v(k)
	where p.chain_id = $1 and p.block > $2 and p.block <= $3 and v.k is not null
), quiet as (
	select n.p,
		f.payee is not null
		and (select count(*) from (select 1 from payment x where x.chain_id = $1 and x.payer = n.p and x.tier in ('A', 'B') limit $4::bigint) s) >= $4::bigint
		and not exists (select 1 from payment x where x.chain_id = $1 and x.payer = n.p and x.tier in ('A', 'B') and x.payee is distinct from f.payee)
		and not exists (select 1 from payment x where x.chain_id = $1 and x.payee = n.p)
		and not exists (select 1 from relayers x where x.r = n.p) as flag
	from named n
	left join lateral (
		select x.payee from payment x where x.chain_id = $1 and x.payer = n.p and x.tier in ('A', 'B') order by x.block, x.log_index limit 1
	) f on true
), grown as (
	select amount, relayer from mint where chain_id = $1 and block > $2 and block <= $3 and relayer is not null group by amount, relayer
), big as (
	select m.amount, m.relayer, count(distinct m.recipient) filter (where m.block <= $2) < $5::int as crossed
	from mint m join grown g on m.chain_id = $1 and m.amount = g.amount and m.relayer = g.relayer
	group by m.amount, m.relayer having count(distinct m.recipient) >= $5::int
), bridged as (
	select distinct m.recipient as p from mint m join big b on m.chain_id = $1 and m.amount = b.amount and m.relayer = b.relayer
	where b.crossed or m.block > $2
)
select p, quiet, bridge from (
	select coalesce(q.p, b.p) as p, coalesce(q.flag, o.no_other_activity, false) as quiet, coalesce(o.bridge_funded, false) or b.p is not null as bridge,
		coalesce(o.no_other_activity, false) as was_quiet, coalesce(o.bridge_funded, false) as was_bridge
	from quiet q full join bridged b on b.p = q.p
	left join payer_flag o on o.chain_id = $1 and o.payer = coalesce(q.p, b.p)
) c
where quiet <> was_quiet or bridge <> was_bridge`
	dropPayerFlagsSQL = `delete from payer_flag f using payer_change c where f.chain_id = $1 and f.payer = c.p`
	addPayerFlagsSQL  = `insert into payer_flag (chain_id, payer, no_other_activity, bridge_funded) select $1, p, quiet, bridge from payer_change where quiet or bridge`
)

func groupFlags(ctx context.Context, tx pgx.Tx, chain int, lo, hi int64, r config.Rules) error {
	return run(ctx, tx,
		stmt{dropPayeeDayFlagsSQL, []any{chain, lo, hi}},
		stmt{addPayeeDayFlagsSQL, []any{chain, lo, hi, r.FixedAmount.MinRows, r.FixedAmount.Share}},
		stmt{dropRelayerDayFlagsSQL, []any{chain, lo, hi}},
		stmt{addRelayerDayFlagsSQL, []any{chain, lo, hi, r.PayeeConcentration.MinRows, r.PayeeConcentration.Share}})
}

func payerFlags(ctx context.Context, tx pgx.Tx, chain int, lo, hi int64, r config.Rules) error {
	return run(ctx, tx,
		stmt{payerChangeSQL, []any{chain, lo, hi, r.NoOtherActivity.MinRows, r.BridgeFunded.MinRecipients}},
		stmt{dropPayerFlagsSQL, []any{chain}},
		stmt{addPayerFlagsSQL, []any{chain}})
}
