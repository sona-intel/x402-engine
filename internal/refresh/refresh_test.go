package refresh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"x402/internal/config"
	"x402/internal/registry"
	"x402/internal/store"
	"x402/internal/testkit"
)

const (
	molandak    = "0x7f6a2850669202519f0fe8aa912451238820db86"
	unlisted    = "0x74dd46f2e70b964895fe21aa70c60a9b61fbe395"
	testRelayer = "0x22c4953c882264cab154ea688004e6dead21eeeb"
	indexerTime = 1791000000
)

func setup(t *testing.T, progress int64) (*testkit.DB, config.Config, *registry.Registry) {
	t.Helper()
	d := testkit.New(t)
	d.Migrate(t)
	d.SetProgress(t, progress, indexerTime)
	cfgs, err := config.Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgs[0]
	cfg.SourceSchema = d.Source
	reg, err := registry.Load("../../config/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return d, cfg, reg
}

func registryFrom(t *testing.T, edits ...string) *registry.Registry {
	t.Helper()
	b, err := os.ReadFile("../../config/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for i := 0; i < len(edits); i += 2 {
		if !strings.Contains(body, edits[i]) {
			t.Fatalf("seed registry has no %q", edits[i])
		}
		body = strings.Replace(body, edits[i], edits[i+1], 1)
	}
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func pay(block int64) testkit.Row {
	return testkit.Row{
		Block: block, LogIndex: 1, BlockTime: indexerTime, Tx: testkit.Hash(int(block)), Method: "eip3009_transfer", Token: testkit.USDC,
		Payer: testkit.Addr(1), Payee: testkit.Addr(2), Nonce: testkit.Hash(int(block) + 1_000_000), Relayer: molandak, Amount: "1000000",
	}
}

func burst(first int64, n int) []testkit.Row {
	return testkit.Burst(first, n, testkit.BurstOpts{Payers: 3, Payee: testkit.Addr(2), Relayer: molandak, Amount: "1000000", BlockTime: indexerTime})
}

func once(t *testing.T, d *testkit.DB, cfg config.Config, reg *registry.Registry) Result {
	t.Helper()
	res, err := Once(t.Context(), d.Store, cfg, reg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func meta(t *testing.T, d *testkit.DB) store.Meta {
	t.Helper()
	m, err := d.Store.Meta(t.Context(), 143)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func scalar[T any](t *testing.T, d *testkit.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := d.Store.Pool.QueryRow(t.Context(), d.SQL(q), args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func column[T any](t *testing.T, d *testkit.DB, q string, args ...any) []T {
	t.Helper()
	rows, err := d.Store.Pool.Query(t.Context(), d.SQL(q), args...)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pgx.CollectRows(rows, pgx.RowTo[T])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func hold(t *testing.T, d *testkit.DB, schema string) {
	t.Helper()
	c, err := d.Store.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), `select pg_advisory_unlock_all()`)
		c.Release()
	})
	if _, err := c.Exec(t.Context(), `select pg_advisory_lock(402143, hashtext($1))`, schema); err != nil {
		t.Fatal(err)
	}
}

func TestNotInitialised(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	if _, err := d.Store.Pool.Exec(t.Context(), d.SQL(`delete from {src}."_meta"`)); err != nil {
		t.Fatal(err)
	}
	if res := once(t, d, cfg, reg); res != (Result{NotInitialised: true}) {
		t.Fatalf("result = %+v", res)
	}
	if meta(t, d).Initialised {
		t.Fatal("meta reads as initialised")
	}
}

func TestNotInitialisedWithoutMetaTable(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	if _, err := d.Store.Pool.Exec(t.Context(), d.SQL(`drop table {src}."_meta"`)); err != nil {
		t.Fatal(err)
	}
	if res := once(t, d, cfg, reg); res != (Result{NotInitialised: true}) {
		t.Fatalf("result = %+v", res)
	}
}

func TestCutoffMargin(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, pay(989), pay(990), pay(991), pay(1000))
	once(t, d, cfg, reg)
	if got := column[int64](t, d, `select block from payment order by block`); !slices.Equal(got, []int64{989, 990}) {
		t.Fatalf("blocks %v", got)
	}
	m := meta(t, d)
	if m.CutoffBlock != 990 || m.WatermarkBlock != 990 || m.IndexerProgressBlock != 1000 {
		t.Fatalf("cutoff %d watermark %d progress %d", m.CutoffBlock, m.WatermarkBlock, m.IndexerProgressBlock)
	}
	if m.AnchorBlock != 990 || scalar[string](t, d, `select '0x' || encode($1::bytea, 'hex')`, m.AnchorTx) != testkit.Hash(990) {
		t.Fatalf("anchor %d %x", m.AnchorBlock, m.AnchorTx)
	}
	d.SetProgress(t, 1010, indexerTime)
	once(t, d, cfg, reg)
	if got := column[int64](t, d, `select block from payment order by block`); !slices.Equal(got, []int64{989, 990, 991, 1000}) {
		t.Fatalf("blocks %v", got)
	}
	if m := meta(t, d); m.WatermarkBlock != 1000 || m.AnchorBlock != 1000 {
		t.Fatalf("watermark %d anchor %d", m.WatermarkBlock, m.AnchorBlock)
	}
}

func TestMetaKeepsTheIndexerFields(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	once(t, d, cfg, reg)
	m := meta(t, d)
	if m.SourceBlock != 1003 || !m.IsReady || m.ProgressTime != indexerTime || !m.Initialised || m.RefreshedAt.IsZero() {
		t.Fatalf("meta = %+v", m)
	}
	if m.RulesVersion != cfg.Rules.Version || m.RegistryVersion != reg.Version || m.RegistryChecksum != reg.Checksum || m.RulesChecksum != cfg.Rules.Checksum() {
		t.Fatalf("versions %q %q checksums %q %q", m.RulesVersion, m.RegistryVersion, m.RulesChecksum, m.RegistryChecksum)
	}
}

func TestMintKeepsLogIndexFromId(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.InsertMints(t, testkit.Mint{Block: 500, LogIndex: 7, BlockTime: indexerTime, Tx: testkit.Hash(1), Recipient: testkit.Addr(3), Relayer: unlisted, Amount: "480379"})
	once(t, d, cfg, reg)
	if block, logIndex := scalar[int64](t, d, `select block from mint`), scalar[int](t, d, `select log_index from mint`); block != 500 || logIndex != 7 {
		t.Fatalf("block %d log index %d", block, logIndex)
	}
	if rows := meta(t, d).RowsMint; rows != 1 {
		t.Fatalf("RowsMint %d", rows)
	}
}

func TestTiers(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	cases := []struct{ name, relayer, method, tier, facilitator, reason string }{
		{"known facilitator", molandak, "eip3009_transfer", "A", "monad-foundation", ""},
		{"unlisted relayer", unlisted, "eip3009_transfer", "B", unlisted, ""},
		{"relayer in no list", testkit.Addr(9), "eip3009_transfer", "B", testkit.Addr(9), ""},
		{"no relayer", "", "eip3009_transfer", "B", "unknown", ""},
		{"receive with authorization", molandak, "eip3009_receive", "X", "", "not_x402"},
		{"batch deposit", molandak, "batch_deposit", "X", "", "not_x402"},
		{"other method", molandak, "other", "X", "", "not_x402"},
		{"test relayer", testRelayer, "permit2_exact", "X", "", "test_relayer"},
		{"test relayer with batch deposit", testRelayer, "batch_deposit", "X", "", "not_x402"},
		{"routed by a known facilitator", molandak, "eip3009_routed", "A", "monad-foundation", ""},
		{"routed by an unlisted relayer", unlisted, "eip3009_routed", "X", "", "routed_unlisted"},
		{"routed by a relayer in no list", testkit.Addr(9), "eip3009_routed", "X", "", "routed_unlisted"},
		{"routed with no relayer", "", "eip3009_routed", "X", "", "routed_unlisted"},
		{"routed by a test relayer", testRelayer, "eip3009_routed", "X", "", "test_relayer"},
	}
	rows := make([]testkit.Row, len(cases))
	for i, c := range cases {
		rows[i] = pay(int64(100 + i))
		rows[i].Relayer, rows[i].Method = c.relayer, c.method
	}
	d.Insert(t, rows...)
	once(t, d, cfg, reg)
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tier, facilitator, reason string
			err := d.Store.Pool.QueryRow(t.Context(), `select tier, coalesce(facilitator_id, ''), coalesce(reason, '') from payment where block = $1`, int64(100+i)).
				Scan(&tier, &facilitator, &reason)
			if err != nil {
				t.Fatal(err)
			}
			if tier != c.tier || facilitator != c.facilitator || reason != c.reason {
				t.Fatalf("got %s %q %q, want %s %q %q", tier, facilitator, reason, c.tier, c.facilitator, c.reason)
			}
		})
	}
	if m := meta(t, d); m.RowsPayment != 14 || m.RowsTierA != 2 || m.RowsTierB != 3 || m.RowsTierX != 9 {
		t.Fatalf("counts %d = %d A + %d B + %d X", m.RowsPayment, m.RowsTierA, m.RowsTierB, m.RowsTierX)
	}
}

func TestTierHonoursRegistryRanges(t *testing.T) {
	d, cfg, _ := setup(t, 1000)
	reg := registryFrom(t,
		"id: monad-foundation", "id: ranged",
		`address: "0x7f6a2850669202519f0fe8aa912451238820db86"`, "address: \"0x7f6a2850669202519f0fe8aa912451238820db86\"\n        from_block: 100\n        to_block: 200")
	d.Insert(t, pay(99), pay(150), pay(201))
	once(t, d, cfg, reg)
	for block, want := range map[int64][2]string{99: {"B", molandak}, 150: {"A", "ranged"}, 201: {"B", molandak}} {
		var tier, facilitator string
		if err := d.Store.Pool.QueryRow(t.Context(), `select tier, facilitator_id from payment where block = $1`, block).Scan(&tier, &facilitator); err != nil {
			t.Fatal(err)
		}
		if tier != want[0] || facilitator != want[1] {
			t.Errorf("block %d got %s %q, want %s %q", block, tier, facilitator, want[0], want[1])
		}
	}
}

func TestMethodOrdinals(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	methods := []string{"eip3009_transfer", "eip3009_receive", "permit2_exact", "permit2_upto", "batch_deposit", "other", "eip3009_routed", "unheard_of"}
	rows := make([]testkit.Row, len(methods))
	for i, m := range methods {
		rows[i] = pay(int64(100 + i))
		rows[i].Method = m
	}
	d.Insert(t, rows...)
	once(t, d, cfg, reg)
	want := []int16{1, 2, 3, 4, 5, 6, 7, 6}
	if got := column[int16](t, d, `select method from payment order by block`); !slices.Equal(got, want) {
		t.Fatalf("ordinals %v, want %v", got, want)
	}
}

func TestPaymentColumnsAreCopied(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	row := testkit.Row{
		Block: 300, LogIndex: 4, BlockTime: 1770136004, Tx: testkit.Hash(1), Method: "eip3009_transfer", Token: testkit.USDC, Payer: testkit.Addr(1),
		Payee: testkit.Addr(2), Nonce: testkit.Hash(2), Relayer: molandak, TxTo: testkit.USDC, Selector: "0xe3ee160e", Amount: "115792089237316195423570985008687907853269984665640564039457584007913129639935",
		ValidAfter: "10", ValidBefore: "20", GasCost: "17283910000000000",
	}
	d.Insert(t, row)
	once(t, d, cfg, reg)
	var got [14]string
	err := d.Store.Pool.QueryRow(t.Context(), `select block::text, log_index::text, block_time::text, day::text, '0x' || encode(tx, 'hex'), '0x' || encode(token, 'hex'),
		'0x' || encode(payer, 'hex'), '0x' || encode(payee, 'hex'), amount::text, '0x' || encode(nonce, 'hex'), valid_after::text || ',' || valid_before::text,
		'0x' || encode(relayer, 'hex') || ',' || '0x' || encode(tx_to, 'hex'), '0x' || encode(selector, 'hex'), gas_cost::text from payment`).
		Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &got[6], &got[7], &got[8], &got[9], &got[10], &got[11], &got[12], &got[13])
	if err != nil {
		t.Fatal(err)
	}
	want := [14]string{"300", "4", "1770136004", "20487", row.Tx, row.Token, row.Payer, row.Payee, row.Amount, row.Nonce, "10,20", row.Relayer + "," + row.TxTo, row.Selector, row.GasCost}
	if got != want {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestRowThatCarriesOnlyAHashCopiesWithNulls(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, testkit.Row{Block: 300, LogIndex: 1, BlockTime: indexerTime, Tx: testkit.Hash(1), Method: "other"})
	once(t, d, cfg, reg)
	nulls := scalar[int](t, d, `select (token is null)::int + (payer is null)::int + (payee is null)::int + (amount is null)::int + (nonce is null)::int +
		(valid_after is null)::int + (valid_before is null)::int + (relayer is null)::int + (tx_to is null)::int + (selector is null)::int + (gas_cost is null)::int from payment`)
	if nulls != 11 {
		t.Fatalf("%d of 11 optional columns are null", nulls)
	}
}

func TestDayBoundary(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	times := []int64{86399, 86400, 1770136004}
	rows := make([]testkit.Row, len(times))
	for i, bt := range times {
		rows[i] = pay(int64(100 + i))
		rows[i].BlockTime = bt
	}
	d.Insert(t, rows...)
	once(t, d, cfg, reg)
	if got := column[int](t, d, `select day from payment order by block`); !slices.Equal(got, []int{0, 1, 20487}) {
		t.Fatalf("days %v", got)
	}
}

func TestBatchEndNeverSplitsABlock(t *testing.T) {
	d, cfg, _ := setup(t, 1000)
	var rows []testkit.Row
	for block := int64(10); block <= 12; block++ {
		for l := 1; l <= 7; l++ {
			r := pay(block)
			r.LogIndex, r.Tx = l, testkit.Hash(int(block)*100+l)
			rows = append(rows, r)
		}
	}
	d.Insert(t, rows...)
	cases := []struct {
		name                          string
		watermark, cutoff, size, want int64
	}{
		{"cut inside a block ends before it", 9, 12, 10, 10},
		{"batch smaller than the first block takes the whole block", 9, 12, 3, 10},
		{"batch filled exactly by the first block takes that block", 10, 12, 7, 11},
		{"fewer rows than the batch ends at the cutoff", 9, 12, 100, 12},
		{"rows above the cutoff do not count", 9, 10, 8, 10},
		{"the row that fills the batch sits in the cutoff block", 9, 11, 14, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg.IngestBatchRows = int(c.size)
			got, err := batchEnd(t.Context(), d.Store.Pool, cfg, c.watermark, c.cutoff)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("batchEnd = %d, want %d", got, c.want)
			}
		})
	}
}

func TestBatchBoundaryMidBlock(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	cfg.IngestBatchRows = 100
	var rows []testkit.Row
	for block := int64(100); block < 136; block++ {
		for l := 1; l <= 7; l++ {
			r := pay(block)
			r.LogIndex, r.Tx = l, testkit.Hash(int(block)*100+l)
			rows = append(rows, r)
		}
	}
	d.Insert(t, rows...)
	if res := once(t, d, cfg, reg); res.Ingested != 252 {
		t.Fatalf("Ingested %d", res.Ingested)
	}
	count := func() (total, distinct int) {
		err := d.Store.Pool.QueryRow(t.Context(), `select count(*), count(distinct (block, log_index)) from payment`).Scan(&total, &distinct)
		if err != nil {
			t.Fatal(err)
		}
		return total, distinct
	}
	if total, distinct := count(); total != 252 || distinct != 252 {
		t.Fatalf("payments %d, distinct %d", total, distinct)
	}
	if res := once(t, d, cfg, reg); res.Ingested != 0 {
		t.Fatalf("second refresh ingested %d", res.Ingested)
	}
	if total, distinct := count(); total != 252 || distinct != 252 {
		t.Fatalf("after the second refresh payments %d, distinct %d", total, distinct)
	}
	if m := meta(t, d); m.RowsPayment != 252 || m.RowsTierA != 252 {
		t.Fatalf("RowsPayment %d RowsTierA %d", m.RowsPayment, m.RowsTierA)
	}
}

func TestResetDetected(t *testing.T) {
	t.Run("source rebuilt under the anchor", func(t *testing.T) {
		d, cfg, reg := setup(t, 500)
		d.Insert(t, burst(391, 100)...)
		once(t, d, cfg, reg)
		if m := meta(t, d); m.WatermarkBlock != 490 || m.RowsPayment != 100 {
			t.Fatalf("watermark %d rows %d", m.WatermarkBlock, m.RowsPayment)
		}
		fresh := burst(391, 100)
		want := make([]string, len(fresh))
		for i := range fresh {
			fresh[i].Tx = testkit.Hash(5000 + i)
			want[i] = fresh[i].Tx
		}
		d.Reset(t)
		d.Insert(t, fresh...)
		d.SetProgress(t, 600, indexerTime)
		if res := once(t, d, cfg, reg); !res.Reset {
			t.Fatal("reset not detected")
		}
		got := column[string](t, d, `select '0x' || encode(tx, 'hex') from payment`)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("payments hold %d hashes, %d new ones expected", len(got), len(want))
		}
		if n := scalar[int](t, d, `select count(*) from payment where tier = 'A' and facilitator_id = 'monad-foundation'`); n != 100 {
			t.Fatalf("%d rows kept tier A", n)
		}
		if m := meta(t, d); m.RowsPayment != 100 || m.WatermarkBlock != 590 {
			t.Fatalf("rows %d watermark %d", m.RowsPayment, m.WatermarkBlock)
		}
	})
	t.Run("indexer progress below the watermark", func(t *testing.T) {
		d, cfg, reg := setup(t, 500)
		d.Insert(t, burst(391, 100)...)
		once(t, d, cfg, reg)
		d.Insert(t, burst(1, 50)...)
		d.SetProgress(t, 50, indexerTime)
		if res := once(t, d, cfg, reg); !res.Reset {
			t.Fatal("reset not detected")
		}
		blocks := column[int64](t, d, `select block from payment order by block`)
		if len(blocks) != 40 || blocks[0] != 1 || blocks[39] != 40 {
			t.Fatalf("blocks %v", blocks)
		}
		if n := scalar[int](t, d, `select count(*) from payment where tier <> 'A'`); n != 0 {
			t.Fatalf("%d rows lost tier A", n)
		}
	})
}

func TestRollbackBelowWatermarkSkips(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, burst(980, 21)...)
	once(t, d, cfg, reg)
	before := column[int64](t, d, `select block from payment order by block`)
	d.SetProgress(t, 995, indexerTime)
	res := once(t, d, cfg, reg)
	if res.Reset || res.Ingested != 0 {
		t.Fatalf("result = %+v", res)
	}
	if m := meta(t, d); m.WatermarkBlock != 990 || m.IndexerProgressBlock != 995 {
		t.Fatalf("watermark %d progress %d", m.WatermarkBlock, m.IndexerProgressBlock)
	}
	if after := column[int64](t, d, `select block from payment order by block`); !slices.Equal(before, after) || len(after) != 11 {
		t.Fatalf("blocks %v, before %v", after, before)
	}
}

func TestVersionChangeRebuilds(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, burst(100, 10)...)
	if res := once(t, d, cfg, reg); res.Reset || res.Ingested != 10 {
		t.Fatalf("first refresh = %+v", res)
	}
	if n := scalar[int](t, d, `select count(*) from payment where tier = 'A'`); n != 10 {
		t.Fatalf("%d rows in tier A", n)
	}
	if n := scalar[int](t, d, `select count(*) from relayer`); n != 12 {
		t.Fatalf("%d relayer rows, want 12", n)
	}
	if n := scalar[int](t, d, `select count(*) from relayer where facilitator_id is not null`); n != 5 {
		t.Fatalf("%d relayer rows with a facilitator, want 5", n)
	}
	before := meta(t, d).RegistryChecksum

	demoted := registryFrom(t, "version: 2026-10-12.1", "version: 2026-10-12.2", "class: facilitator", "class: unlisted")
	if res := once(t, d, cfg, demoted); res.Reset || res.Ingested != 10 {
		t.Fatalf("version change = %+v", res)
	}
	if n := scalar[int](t, d, `select count(*) from payment where tier = 'B' and facilitator_id = $1`, molandak); n != 10 {
		t.Fatalf("%d rows in tier B", n)
	}
	m := meta(t, d)
	if m.RegistryChecksum == before || m.RegistryChecksum != demoted.Checksum || m.RegistryVersion != "2026-10-12.2" || m.RowsPayment != 10 || m.RowsTierB != 10 || m.RowsTierA != 0 {
		t.Fatalf("meta %+v", m)
	}
	if n := scalar[int](t, d, `select count(*) from registry_version`); n != 2 {
		t.Fatalf("registry_version holds %d rows, want 2", n)
	}

	edited := registryFrom(t, "version: 2026-10-12.1", "version: 2026-10-12.2", "class: facilitator", "class: unlisted",
		"0x97316fa4730bc7d3b295234f8e4d04a0a4c093e8", "0x97316fa4730bc7d3b295234f8e4d04a0a4c093e9")
	once(t, d, cfg, edited)
	if n := scalar[int](t, d, `select count(*) from registry_version`); n != 2 {
		t.Fatalf("registry_version holds %d rows after a content change, want 2", n)
	}
	if got := scalar[string](t, d, `select checksum from registry_version where version = '2026-10-12.2'`); got != edited.Checksum || got == demoted.Checksum {
		t.Fatalf("stored checksum %s, want %s", got, edited.Checksum)
	}
	if n := scalar[int](t, d, `select count(*) from payment`); n != 10 {
		t.Fatalf("%d payments after the content change", n)
	}
}

func TestRulesChangeRebuilds(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, burst(100, 10)...)
	once(t, d, cfg, reg)
	before := meta(t, d).RulesChecksum
	cfg.Rules.Version = "2026-10-12.2"
	cfg.Rules.FixedAmount.MinRows = 101
	if res := once(t, d, cfg, reg); res.Reset || res.Ingested != 10 {
		t.Fatalf("rules change = %+v", res)
	}
	if m := meta(t, d); m.RulesChecksum == before || m.RulesChecksum != cfg.Rules.Checksum() || m.RulesVersion != "2026-10-12.2" || m.RowsPayment != 10 {
		t.Fatalf("meta %+v", m)
	}
}

func TestTwoRefreshesDoNotOverlap(t *testing.T) {
	t.Run("same engine schema", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		hold(t, d, d.Engine+":eip155:143")
		if _, err := Once(t.Context(), d.Store, cfg, reg); !errors.Is(err, ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
	})
	t.Run("another engine schema", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		hold(t, d, "another_schema")
		once(t, d, cfg, reg)
	})
	t.Run("released after a round", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		once(t, d, cfg, reg)
		once(t, d, cfg, reg)
	})
}

func TestLoopCallsDoneAfterEveryRoundAndStopsOnCancel(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	cfg.RefreshInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rounds := 0
	Loop(ctx, d.Store, cfg, reg, func(_ Result, err error) {
		if err != nil {
			t.Error(err)
		}
		if rounds++; rounds == 3 {
			cancel()
		}
	})
	if rounds != 3 {
		t.Fatalf("done called %d times, want 3", rounds)
	}
}

func TestLoopSkipsRoundsThatFindTheLockHeld(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	cfg.RefreshInterval = 5 * time.Millisecond
	hold(t, d, d.Engine+":eip155:143")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	Loop(ctx, d.Store, cfg, reg, func(Result, error) { t.Error("done called for a skipped round") })
}
