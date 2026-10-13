package refresh

import (
	"context"

	"github.com/jackc/pgx/v5"

	"x402/internal/config"
)

type stmt struct {
	sql  string
	args []any
}

func run(ctx context.Context, tx pgx.Tx, stmts ...stmt) error {
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}

func derive(ctx context.Context, tx pgx.Tx, chain int, lo, hi int64, rules config.Rules) (int, error) {
	if err := groupFlags(ctx, tx, chain, lo, hi, rules); err != nil {
		return 0, err
	}
	if err := payerFlags(ctx, tx, chain, lo, hi, rules); err != nil {
		return 0, err
	}
	return rebuildDays(ctx, tx, chain, lo, hi, rules.VolumeToken)
}
