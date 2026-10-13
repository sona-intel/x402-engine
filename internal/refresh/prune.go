package refresh

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"x402/internal/config"
	"x402/internal/store"
)

type Pruned struct {
	Payments int64
	Source   int64
}

const (
	pruneEngineSQL = `
with d as (delete from payment where chain_id = $1 and day < $2 returning tier)
select count(*), count(*) filter (where tier = 'A'), count(*) filter (where tier = 'B'), count(*) filter (where tier = 'X') from d`
	pruneMetaSQL = `
update meta set rows_payment = rows_payment - $2, rows_tier_a = rows_tier_a - $3, rows_tier_b = rows_tier_b - $4, rows_tier_x = rows_tier_x - $5
where chain_id = $1`
	prunePayeeFlagsSQL   = `delete from payee_day_flag where chain_id = $1 and day < $2`
	pruneRelayerFlagsSQL = `delete from relayer_day_flag where chain_id = $1 and day < $2`
	// rows past the watermark stay because the engine has not copied them yet
	pruneSourceSQL = `delete from {src}."X402Payment" where "chainId" = $1 and "blockTime" < $2 and "blockNumber" <= $3`
)

func Prune(ctx context.Context, s *store.Store, cfg config.Config, now time.Time) (Pruned, error) {
	var out Pruned
	if cfg.RetentionDays <= 0 {
		return out, nil
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, lockSQL, cfg.Chain).Scan(&locked); err != nil {
		return out, err
	}
	if !locked {
		return out, ErrBusy
	}
	defer unlock(ctx, conn, cfg.Chain)

	// whole days only, because a day cut in half would be rebuilt from its remaining rows
	cutoffDay := now.Unix()/86400 - int64(cfg.RetentionDays)
	m, err := s.Meta(ctx, cfg.ChainID)
	if err != nil {
		return out, err
	}
	// a day pruned while the watermark is inside it would be rebuilt from its remaining rows only
	if !m.Initialised || m.WatermarkBlock < m.CutoffBlock {
		return out, nil
	}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var a, b, x int64
		if err := tx.QueryRow(ctx, pruneEngineSQL, cfg.ChainID, cutoffDay).Scan(&out.Payments, &a, &b, &x); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, pruneMetaSQL, cfg.ChainID, out.Payments, a, b, x); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, prunePayeeFlagsSQL, cfg.ChainID, cutoffDay); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, pruneRelayerFlagsSQL, cfg.ChainID, cutoffDay)
		return err
	})
	if err != nil {
		return Pruned{}, err
	}
	var present bool
	if err := conn.QueryRow(ctx, store.SQL(cfg.SourceSchema, hasSourceSQL)).Scan(&present); err != nil || !present {
		return out, err
	}
	tag, err := conn.Exec(ctx, store.SQL(cfg.SourceSchema, pruneSourceSQL), cfg.ChainID, cutoffDay*86400, m.WatermarkBlock)
	out.Source = tag.RowsAffected()
	return out, err
}

func PruneLoop(ctx context.Context, s *store.Store, cfg config.Config, interval time.Duration, done func(Pruned, error)) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		res, err := Prune(ctx, s, cfg, time.Now())
		if ctx.Err() != nil {
			return
		}
		wait = interval
		if errors.Is(err, ErrBusy) {
			wait = time.Minute
			continue
		}
		done(res, err)
	}
}
