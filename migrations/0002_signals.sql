create view payment_signals as
select s.*,
    case when s.tier = 'X' then null
        else not s.self_payment and (s.fixed_amount::int + s.payee_concentration::int + s.no_other_activity::int + s.bridge_funded::int) < 2
    end as genuine
from (
    select p.*,
        p.tier in ('A', 'B') and coalesce(p.amount = d.fixed_amount, false) as fixed_amount,
        p.tier in ('A', 'B') and coalesce(p.payee = r.payee, false) as payee_concentration,
        p.tier in ('A', 'B') and coalesce(f.no_other_activity, false) as no_other_activity,
        p.tier in ('A', 'B') and coalesce(f.bridge_funded, false) as bridge_funded,
        p.tier in ('A', 'B') and coalesce(p.payer = p.payee, false) as self_payment
    from payment p
    left join payee_day_flag d on d.chain_id = p.chain_id and d.payee = p.payee and d.day = p.day
    left join relayer_day_flag r on r.chain_id = p.chain_id and r.relayer = p.relayer and r.day = p.day
    left join payer_flag f on f.chain_id = p.chain_id and f.payer = p.payer
) s;
