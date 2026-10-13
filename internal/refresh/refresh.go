package refresh

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"x402/internal/config"
	"x402/internal/registry"
	"x402/internal/store"
)

type Result struct {
	Ingested       int
	DirtyDays      int
	Reset          bool
	NotInitialised bool
}

var ErrBusy = errors.New("refresh already running")

type indexerState struct {
	progress, sourceBlock, progressTime int64
	ready                               bool
}

const (
	lockSQL       = `select pg_try_advisory_lock(402143, hashtext(current_schema() || ':' || $1::text))`
	unlockSQL     = `select pg_advisory_unlock(402143, hashtext(current_schema() || ':' || $1::text))`
	ensureMetaSQL = `insert into meta (chain_id) values ($1) on conflict do nothing`
	notReadySQL   = `update meta set initialised = false, refreshed_at = now() where chain_id = $1`
	finishSQL     = `
update meta set indexer_progress_block = $2, source_block = $3, is_ready = $4, progress_time = $5, cutoff_block = $6, refreshed_at = now(),
	refresh_ms = $7, initialised = true
where chain_id = $1`
)

var (
	rowTables   = []string{"payment", "mint", "payer_flag", "payee_day_flag", "relayer_day_flag"}
	dailyTables = []string{"daily_facilitator", "daily_seller", "daily_payer", "daily_flow", "daily_rules"}
)

// a day with no payment row left is a pruned day and its totals cannot be rebuilt, and a null minimum deletes nothing
const (
	firstDaySQL     = `select min(day) from payment where chain_id = $1`
	vacuumSQL       = `vacuum payment, mint`
	dropDailyFormat = "delete from %s where chain_id = $1 and day >= $2"
)

const (
	hasSourceSQL  = `select to_regclass('{src}."_meta"') is not null`
	readSourceSQL = `
select "progressBlock", "sourceBlock", "isReady", coalesce(extract(epoch from "progressBlockTime")::bigint, 0)
from {src}."_meta" where "chainId" = $1`
	anchorSQL = `
select exists (select 1 from {src}."X402Payment" where "chainId" = $1 and "blockNumber" = $2 and "txHash" = '0x' || encode($3::bytea, 'hex'))`
)

const (
	resetSQL = `
update meta set watermark_block = -1, anchor_block = -1, anchor_tx = null, rows_payment = 0, rows_tier_a = 0, rows_tier_b = 0, rows_tier_x = 0,
	rows_mint = 0
where chain_id = $1`
	dropRelayersSQL = `delete from relayer where chain_id = $1`
	dropCodesSQL    = `delete from facilitator_code where chain_id = $1`
	insertCodesSQL  = `
insert into facilitator_code (chain_id, code, facilitator_id) select $1, c, i from unnest($2::text[], $3::text[]) as u(c, i)`
	insertRelayersSQL = `
insert into relayer (chain_id, address, from_block, to_block, facilitator_id, class)
select $1, decode(substr(a, 3), 'hex'), f, t, case when c = 'facilitator' then i end, c
from unnest($2::text[], $3::bigint[], $4::bigint[], $5::text[], $6::text[]) as u(a, f, t, i, c)`
	auditSQL = `
insert into registry_version (chain_id, version, checksum) values ($1, $2, $3)
on conflict (chain_id, version) do update set checksum = excluded.checksum, loaded_at = now()`
	versionsSQL = `
update meta set registry_version = $2, registry_checksum = $3, rules_version = $4, rules_checksum = $5 where chain_id = $1`
)

func Once(ctx context.Context, s *store.Store, cfg config.Config, reg *registry.Registry) (Result, error) {
	start := time.Now()
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, lockSQL, cfg.Chain).Scan(&locked); err != nil {
		return Result{}, err
	}
	if !locked {
		return Result{}, ErrBusy
	}
	defer unlock(ctx, conn, cfg.Chain)
	return round(ctx, s, conn, cfg, reg, start)
}

func Loop(ctx context.Context, s *store.Store, cfg config.Config, reg *registry.Registry, done func(Result, error)) {
	tick := time.NewTicker(cfg.RefreshInterval)
	defer tick.Stop()
	for ctx.Err() == nil {
		res, err := Once(ctx, s, cfg, reg)
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, ErrBusy) {
			done(res, err)
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func unlock(ctx context.Context, conn *pgxpool.Conn, chain string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var released bool
	if err := conn.QueryRow(ctx, unlockSQL, chain).Scan(&released); err != nil || !released {
		// closing the session is the only sure way to drop a lock that would not release
		_ = conn.Conn().Close(ctx)
	}
}

func round(ctx context.Context, s *store.Store, conn *pgxpool.Conn, cfg config.Config, reg *registry.Registry, start time.Time) (Result, error) {
	if _, err := conn.Exec(ctx, ensureMetaSQL, cfg.ChainID); err != nil {
		return Result{}, err
	}
	ix, ok, err := readIndexer(ctx, conn, cfg)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		_, err := conn.Exec(ctx, notReadySQL, cfg.ChainID)
		return Result{NotInitialised: true}, err
	}
	m, err := s.Meta(ctx, cfg.ChainID)
	if err != nil {
		return Result{}, err
	}
	var res Result
	if res.Reset, err = indexerWasReset(ctx, conn, cfg, m, ix.progress); err != nil {
		return res, err
	}
	rulesSum := cfg.Rules.Checksum()
	changed := reg.Checksum != m.RegistryChecksum || rulesSum != m.RulesChecksum
	if res.Reset || changed {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			var first *int32
			if err := tx.QueryRow(ctx, firstDaySQL, cfg.ChainID).Scan(&first); err != nil {
				return err
			}
			for _, t := range dailyTables {
				if _, err := tx.Exec(ctx, fmt.Sprintf(dropDailyFormat, t), cfg.ChainID, first); err != nil {
					return err
				}
			}
			for _, t := range rowTables {
				if _, err := tx.Exec(ctx, "delete from "+t+" where chain_id = $1", cfg.ChainID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, resetSQL, cfg.ChainID); err != nil {
				return err
			}
			if !changed {
				return nil
			}
			return loadRegistry(ctx, tx, cfg, reg, rulesSum)
		})
		if err != nil {
			return res, err
		}
		m.WatermarkBlock = -1
		// dead tuples of the wipe would otherwise double the tables during the re-ingest
		if _, err := conn.Exec(ctx, vacuumSQL); err != nil {
			return res, err
		}
	}
	if err := backfillRollups(ctx, conn, cfg.ChainID, cfg.Rules.VolumeToken); err != nil {
		return res, err
	}
	cutoff := ix.progress - cfg.MarginBlocks
	for m.WatermarkBlock < cutoff {
		hi, err := batchEnd(ctx, conn, cfg, m.WatermarkBlock, cutoff)
		if err != nil {
			return res, err
		}
		n, dirty, err := batch(ctx, conn, cfg, m.WatermarkBlock, hi)
		if err != nil {
			return res, err
		}
		res.Ingested += n
		res.DirtyDays += dirty
		m.WatermarkBlock = hi
	}
	_, err = conn.Exec(ctx, finishSQL, cfg.ChainID, ix.progress, ix.sourceBlock, ix.ready, ix.progressTime, cutoff, time.Since(start).Milliseconds())
	return res, err
}

func readIndexer(ctx context.Context, conn *pgxpool.Conn, cfg config.Config) (indexerState, bool, error) {
	var ix indexerState
	var present bool
	if err := conn.QueryRow(ctx, store.SQL(cfg.SourceSchema, hasSourceSQL)).Scan(&present); err != nil {
		return ix, false, err
	}
	if !present {
		return ix, false, nil
	}
	err := conn.QueryRow(ctx, store.SQL(cfg.SourceSchema, readSourceSQL), cfg.ChainID).Scan(&ix.progress, &ix.sourceBlock, &ix.ready, &ix.progressTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return ix, false, nil
	}
	return ix, err == nil, err
}

func indexerWasReset(ctx context.Context, conn *pgxpool.Conn, cfg config.Config, m store.Meta, progress int64) (bool, error) {
	if m.WatermarkBlock > progress {
		return true, nil
	}
	if m.AnchorTx == nil {
		return false, nil
	}
	var found bool
	if err := conn.QueryRow(ctx, store.SQL(cfg.SourceSchema, anchorSQL), cfg.ChainID, m.AnchorBlock, m.AnchorTx).Scan(&found); err != nil {
		return false, err
	}
	return !found, nil
}

func loadRegistry(ctx context.Context, tx pgx.Tx, cfg config.Config, reg *registry.Registry, rulesSum string) error {
	var addrs, ids, classes []string
	var from []int64
	var to []*int64
	for _, e := range reg.Entries {
		for _, a := range e.Addresses {
			var first int64
			if a.FromBlock != nil {
				first = *a.FromBlock
			}
			addrs, ids, classes = append(addrs, a.Hex), append(ids, e.ID), append(classes, e.Class)
			from, to = append(from, first), append(to, a.ToBlock)
		}
	}
	if _, err := tx.Exec(ctx, dropRelayersSQL, cfg.ChainID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertRelayersSQL, cfg.ChainID, addrs, from, to, ids, classes); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, dropCodesSQL, cfg.ChainID); err != nil {
		return err
	}
	codes, owners := reg.Codes()
	if _, err := tx.Exec(ctx, insertCodesSQL, cfg.ChainID, codes, owners); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, auditSQL, cfg.ChainID, reg.Version, reg.Checksum); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, versionsSQL, cfg.ChainID, reg.Version, reg.Checksum, cfg.Rules.Version, rulesSum)
	return err
}

// analyzeRows is the batch size from which the planner needs fresh statistics, because a chain that is new to the tables has none
var analyzeRows = 10_000

func batch(ctx context.Context, conn *pgxpool.Conn, cfg config.Config, lo, hi int64) (ingested, dirty int, err error) {
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		c, err := ingest(ctx, tx, cfg, lo, hi)
		if err != nil {
			return err
		}
		if c.payments+c.mints >= int64(analyzeRows) {
			if _, err := tx.Exec(ctx, "analyze payment, mint"); err != nil {
				return err
			}
		}
		if dirty, err = derive(ctx, tx, cfg.ChainID, lo, hi, cfg.Rules); err != nil {
			return err
		}
		ingested = int(c.payments + c.mints)
		return advance(ctx, tx, cfg.ChainID, lo, hi, c)
	})
	return ingested, dirty, err
}
