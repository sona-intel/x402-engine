package migrate_test

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"x402/internal/migrate"
	"x402/internal/testkit"
	"x402/migrations"
)

func queryStrings(t *testing.T, d *testkit.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := d.Store.Pool.Query(t.Context(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func fileVersions(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestUpCreatesTablesAndIsIdempotent(t *testing.T) {
	d := testkit.New(t)
	for range 2 {
		if err := migrate.Up(t.Context(), d.Store.Pool); err != nil {
			t.Fatal(err)
		}
	}
	got := queryStrings(t, d, `select table_name from information_schema.tables where table_schema = $1 and table_type = 'BASE TABLE' order by 1`, d.Engine)
	want := []string{
		"daily_facilitator", "daily_flow", "daily_payer", "daily_rules", "daily_seller", "facilitator_code", "meta", "mint", "payee_day_flag", "payer_flag",
		"payment", "registry_version", "relayer", "relayer_day_flag", "schema_migrations",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tables %q, want %q", got, want)
	}
	t.Run("recorded_versions_equal_file_names", func(t *testing.T) {
		if v := queryStrings(t, d, `select version from schema_migrations order by 1`); !slices.Equal(v, fileVersions(t)) {
			t.Fatalf("versions %q", v)
		}
	})
}

func TestPaymentIndexes(t *testing.T) {
	d := testkit.New(t)
	d.Migrate(t)
	got := queryStrings(t, d, `select regexp_replace(indexdef, '^.*\(', '(') collate "C" from pg_indexes where schemaname = $1 and tablename = 'payment' and indexname <> 'payment_pkey' order by 1`, d.Engine)
	want := []string{
		"(chain_id, day, block, log_index)", "(chain_id, facilitator_id, block, log_index)", "(chain_id, method, block, log_index)",
		"(chain_id, payee, block, log_index)", "(chain_id, payer, block, log_index)", "(chain_id, relayer, block, log_index)", "(chain_id, tx)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("indexes %q, want %q", got, want)
	}
}

func TestMintIndexes(t *testing.T) {
	d := testkit.New(t)
	d.Migrate(t)
	got := queryStrings(t, d, `select regexp_replace(indexdef, '^.*\(', '(') collate "C" from pg_indexes where schemaname = $1 and tablename = 'mint' and indexname <> 'mint_pkey' order by 1`, d.Engine)
	want := []string{"(chain_id, amount, relayer)", "(chain_id, recipient)"}
	if !slices.Equal(got, want) {
		t.Fatalf("indexes %q, want %q", got, want)
	}
}

func TestMigrateLeavesOtherSchemasAlone(t *testing.T) {
	d := testkit.New(t)
	const q = `select table_name collate "C" from information_schema.tables where table_schema = $1 order by 1`
	before := queryStrings(t, d, q, d.Source)
	d.Migrate(t)
	if after := queryStrings(t, d, q, d.Source); !slices.Equal(before, after) {
		t.Fatalf("source tables %q, want %q", after, before)
	}
}

func TestDailyEntityIndexes(t *testing.T) {
	d := testkit.New(t)
	d.Migrate(t)
	for table, want := range map[string][]string{
		"daily_seller": {"(chain_id, seller, day)"}, "daily_payer": {"(chain_id, payer, day)"}, "daily_facilitator": nil,
	} {
		got := queryStrings(t, d, `select regexp_replace(indexdef, '^.*\(', '(') from pg_indexes where schemaname = $1 and tablename = $2 and indexname <> $2 || '_pkey'`, d.Engine, table)
		if !slices.Equal(got, want) {
			t.Errorf("%s indexes %q, want %q", table, got, want)
		}
	}
}
