package refresh

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"x402/internal/config"
	"x402/internal/store"
)

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type counts struct {
	payments, tierA, tierB, tierX, mints int64
}

const batchEndSQL = `
select "blockNumber" from {src}."X402Payment"
where "chainId" = $1 and "blockNumber" > $2 and "blockNumber" <= $3
order by "blockNumber" offset $4 limit 1`

const insertPaymentsSQL = `
with s as (
	select p."blockNumber" as block, p."logIndex" as log_index, p."blockTime" as block_time, p."txHash" as tx, p.token, p.payer, p.payee,
		p.amount, p.nonce, p."validAfter" as valid_after, p."validBefore" as valid_before, p.relayer, p."txTo" as tx_to, p.selector,
		p."gasCost" as gas_cost, decode(substr(p.relayer, 3), 'hex') as relayer_bytes, {codes} as codes,
		case p.method when 'eip3009_transfer' then 1 when 'eip3009_receive' then 2 when 'permit2_exact' then 3 when 'permit2_upto' then 4
			when 'batch_deposit' then 5 when 'eip3009_routed' then 7 else 6 end as method
	from {src}."X402Payment" p
	where p."chainId" = $1 and p."blockNumber" > $2 and p."blockNumber" <= $3
), ins as (
	insert into payment (chain_id, block, log_index, block_time, day, tx, method, token, payer, payee, amount, nonce, valid_after, valid_before,
		relayer, tx_to, selector, gas_cost, tier, facilitator_id, reason)
	select $1, s.block, s.log_index, s.block_time, s.block_time / 86400, decode(substr(s.tx, 3), 'hex'), s.method,
		decode(substr(s.token, 3), 'hex'), decode(substr(s.payer, 3), 'hex'), decode(substr(s.payee, 3), 'hex'), s.amount,
		decode(substr(s.nonce, 3), 'hex'), s.valid_after, s.valid_before, s.relayer_bytes, decode(substr(s.tx_to, 3), 'hex'),
		decode(substr(s.selector, 3), 'hex'), s.gas_cost,
		case when x.reason is not null then 'X' when f.id is not null then 'A' else 'B' end,
		case when x.reason is not null then null when f.id is not null then f.id else coalesce(lower(s.relayer), 'unknown') end,
		x.reason
	from s
	left join lateral (
		select r.class, r.facilitator_id from relayer r
		where r.chain_id = $1 and r.address = s.relayer_bytes and s.block between r.from_block and coalesce(r.to_block, 9223372036854775807)
		order by r.from_block desc limit 1
	) l on true
	left join lateral (
		select fc.facilitator_id from facilitator_code fc
		where fc.chain_id = $1 and l.class = 'facilitator' and fc.facilitator_id = l.facilitator_id and s.codes is not null
			and fc.code = any(string_to_array(translate(s.codes, '|=', ',,'), ','))
		limit 1
	) k on true
	cross join lateral (
		select case when l.class = 'facilitator' and (k.facilitator_id is not null or not exists (
			select 1 from facilitator_code fc where fc.chain_id = $1 and fc.facilitator_id = l.facilitator_id
		)) then l.facilitator_id end as id
	) f
	cross join lateral (
		select case when s.method in (2, 5, 6) then 'not_x402' when l.class = 'test' then 'test_relayer'
			when s.method = 7 and f.id is null then 'routed_unlisted' end as reason
	) x
	returning tier
)
select count(*), count(*) filter (where tier = 'A'), count(*) filter (where tier = 'B'), count(*) filter (where tier = 'X') from ins`

const insertMintsSQL = `
insert into mint (chain_id, block, log_index, block_time, recipient, amount, relayer, tx_to)
select $1, "blockNumber", split_part(id, '_', 3)::int, "blockTime", decode(substr(recipient, 3), 'hex'), amount,
	decode(substr(relayer, 3), 'hex'), decode(substr("txTo", 3), 'hex')
from {src}."UsdcMint"
where "chainId" = $1 and "blockNumber" > $2 and "blockNumber" <= $3`

const lastPaymentSQL = `
select block, tx from payment where chain_id = $1 and block > $2 and block <= $3 order by block desc, log_index desc limit 1`

const advanceSQL = `
update meta set watermark_block = $2, anchor_block = coalesce($3, anchor_block), anchor_tx = coalesce($4, anchor_tx),
	rows_payment = rows_payment + $5, rows_tier_a = rows_tier_a + $6, rows_tier_b = rows_tier_b + $7, rows_tier_x = rows_tier_x + $8,
	rows_mint = rows_mint + $9
where chain_id = $1`

func batchEnd(ctx context.Context, q queryer, cfg config.Config, watermark, cutoff int64) (int64, error) {
	var b int64
	err := q.QueryRow(ctx, store.SQL(cfg.SourceSchema, batchEndSQL), cfg.ChainID, watermark, cutoff, cfg.IngestBatchRows-1).Scan(&b)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return cutoff, nil
	case err != nil:
		return 0, err
	case b-1 > watermark:
		return b - 1, nil
	}
	return b, nil
}

func ingest(ctx context.Context, tx pgx.Tx, cfg config.Config, lo, hi int64) (counts, error) {
	var c counts
	codes := "null::text"
	if cfg.BuilderCodes {
		codes = `p."builderCodes"`
	}
	q := strings.ReplaceAll(insertPaymentsSQL, "{codes}", codes)
	err := tx.QueryRow(ctx, store.SQL(cfg.SourceSchema, q), cfg.ChainID, lo, hi).Scan(&c.payments, &c.tierA, &c.tierB, &c.tierX)
	if err != nil {
		return c, err
	}
	tag, err := tx.Exec(ctx, store.SQL(cfg.SourceSchema, insertMintsSQL), cfg.ChainID, lo, hi)
	c.mints = tag.RowsAffected()
	return c, err
}

func advance(ctx context.Context, tx pgx.Tx, chain int, lo, hi int64, c counts) error {
	var block *int64
	var hash []byte
	err := tx.QueryRow(ctx, lastPaymentSQL, chain, lo, hi).Scan(&block, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, advanceSQL, chain, hi, block, hash, c.payments, c.tierA, c.tierB, c.tierX, c.mints)
	return err
}
