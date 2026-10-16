package store_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"x402/internal/store"
	"x402/internal/testkit"
)

func TestSQLQuotesTheSourceSchema(t *testing.T) {
	got := store.SQL("src_ab12", `select 1 from {src}."X402Payment"`)
	want := `select 1 from "src_ab12"."X402Payment"`
	if got != want {
		t.Fatalf("SQL = %q, want %q", got, want)
	}
}

func TestMethodNamesRoundTripAndUnknownOrdinalsAreOther(t *testing.T) {
	for ordinal, name := range map[int16]string{1: "eip3009_transfer", 6: "other", 7: "eip3009_routed", 0: "other", 8: "other", -1: "other"} {
		if got := store.MethodName(ordinal); got != name {
			t.Errorf("MethodName(%d) = %q, want %q", ordinal, got, name)
		}
	}
	if o, ok := store.MethodOrdinal("eip3009_routed"); !ok || o != 7 {
		t.Fatalf("MethodOrdinal(eip3009_routed) = %d %v", o, ok)
	}
}

func TestMetaOfAnEmptyEngineIsTheStartingState(t *testing.T) {
	d := testkit.New(t)
	d.Migrate(t)
	m, err := d.Store.Meta(t.Context(), 143)
	if err != nil {
		t.Fatal(err)
	}
	if m.ChainID != 143 || m.WatermarkBlock != -1 || m.Initialised || m.RowsPayment != 0 {
		t.Fatalf("meta of an empty engine = %+v", m)
	}
}

func TestMetaReadsEveryColumn(t *testing.T) {
	d := testkit.New(t)
	d.Migrate(t)
	_, err := d.Store.Pool.Exec(t.Context(), `insert into meta (chain_id, watermark_block, anchor_block, anchor_tx, registry_version, registry_checksum,
		rules_version, rules_checksum, indexer_progress_block, source_block, is_ready, progress_time, cutoff_block, initialised,
		refresh_ms, rows_payment, rows_tier_a, rows_tier_b, rows_tier_x, rows_mint)
		values (143, 100, 99, '\xab', 'r1', 'rc', 'u1', 'uc', 110, 113, true, 1770136004, 100, true, 42, 7, 4, 2, 1, 5)`)
	if err != nil {
		t.Fatal(err)
	}
	m, err := d.Store.Meta(t.Context(), 143)
	if err != nil {
		t.Fatal(err)
	}
	if m.WatermarkBlock != 100 || m.AnchorBlock != 99 || len(m.AnchorTx) != 1 || m.RegistryVersion != "r1" || m.RegistryChecksum != "rc" ||
		m.RulesVersion != "u1" || m.RulesChecksum != "uc" || m.IndexerProgressBlock != 110 || m.SourceBlock != 113 || !m.IsReady ||
		m.ProgressTime != 1770136004 || m.CutoffBlock != 100 || !m.Initialised || m.RefreshMS != 42 || m.RowsPayment != 7 ||
		m.RowsTierA != 4 || m.RowsTierB != 2 || m.RowsTierX != 1 || m.RowsMint != 5 || m.RefreshedAt.IsZero() {
		t.Fatalf("meta = %+v", m)
	}
}

func TestConnectWithoutDSNReadsLibpqEnvironment(t *testing.T) {
	dsn := os.Getenv("X402_TEST_DSN")
	if dsn == "" {
		t.Skip("X402_TEST_DSN is not set")
	}
	cc, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGHOST", cc.Host)
	t.Setenv("PGPORT", strconv.Itoa(int(cc.Port)))
	t.Setenv("PGUSER", cc.User)
	t.Setenv("PGDATABASE", cc.Database)
	s, err := store.Connect(t.Context(), "", "eng_probe")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	var path string
	if err := s.Pool.QueryRow(t.Context(), `show search_path`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != `"eng_probe"` || s.Pool.Config().MaxConns != 8 {
		t.Fatalf("search_path %q, MaxConns %d", path, s.Pool.Config().MaxConns)
	}
	var workMem string
	if err := s.Pool.QueryRow(t.Context(), `show work_mem`).Scan(&workMem); err != nil || workMem != "16MB" {
		t.Fatalf("work_mem %q, err %v", workMem, err)
	}
}
