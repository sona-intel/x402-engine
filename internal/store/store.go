package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

func Connect(ctx context.Context, dsn, engineSchema string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres config: %w", err)
	}
	cfg.MaxConns = 8
	// the flows aggregate sorts about 34k groups, which spills to disk at the 4MB default
	cfg.ConnConfig.RuntimeParams["work_mem"] = "16MB"
	cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{engineSchema}.Sanitize()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres pool: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func SQL(sourceSchema, q string) string {
	return strings.ReplaceAll(q, "{src}", pgx.Identifier{sourceSchema}.Sanitize())
}

type Meta struct {
	ChainID              int       `db:"chain_id"`
	WatermarkBlock       int64     `db:"watermark_block"`
	AnchorBlock          int64     `db:"anchor_block"`
	AnchorTx             []byte    `db:"anchor_tx"`
	RegistryVersion      string    `db:"registry_version"`
	RegistryChecksum     string    `db:"registry_checksum"`
	RulesVersion         string    `db:"rules_version"`
	RulesChecksum        string    `db:"rules_checksum"`
	IndexerProgressBlock int64     `db:"indexer_progress_block"`
	SourceBlock          int64     `db:"source_block"`
	IsReady              bool      `db:"is_ready"`
	ProgressTime         int64     `db:"progress_time"`
	CutoffBlock          int64     `db:"cutoff_block"`
	Initialised          bool      `db:"initialised"`
	RefreshedAt          time.Time `db:"refreshed_at"`
	RefreshMS            int64     `db:"refresh_ms"`
	RowsPayment          int64     `db:"rows_payment"`
	RowsTierA            int64     `db:"rows_tier_a"`
	RowsTierB            int64     `db:"rows_tier_b"`
	RowsTierX            int64     `db:"rows_tier_x"`
	RowsMint             int64     `db:"rows_mint"`
}

func (s *Store) Meta(ctx context.Context, chain int) (Meta, error) {
	rows, err := s.Pool.Query(ctx, `select * from meta where chain_id = $1`, chain)
	if err != nil {
		return Meta{}, err
	}
	m, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Meta])
	if errors.Is(err, pgx.ErrNoRows) {
		// no refresh has run yet, so the chain reads as untouched
		return Meta{ChainID: chain, WatermarkBlock: -1, AnchorBlock: -1, CutoffBlock: -1}, nil
	}
	return m, err
}
