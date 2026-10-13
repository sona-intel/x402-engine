package refresh

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"x402/internal/registry"
	"x402/internal/testkit"
)

const (
	seller         = "0x1995159c8d4df2268a17c0169a80f0e7d11a7424"
	minter         = "0x99f5a2e5d81f22ddb130f4a194584532a3dc3c2e"
	strayPayee     = "0x93053f1e7a5efeda532fe69cbbe43cbec3a0f13f"
	selfPayer      = "0x940ecb27d8f4f7009c8bc12969496e9639717638"
	otherToken     = "0x00000000efe302beaa2b3e6e1b18d08d69a9012a"
	secondMolandak = "0x7f6a2850669202519f0fe8aa912451238820db87"
	maxUint256     = "115792089237316195423570985008687907853269984665640564039457584007913129639935"
)

const verdictsSQL = `
select block, concat_ws(',', case when fixed_amount then 'fixed_amount' end, case when payee_concentration then 'payee_concentration' end,
	case when no_other_activity then 'no_other_activity' end, case when bridge_funded then 'bridge_funded' end,
	case when self_payment then 'self_payment' end), genuine
from payment_signals order by block, log_index`

type verdict struct {
	Block   int64
	Fired   string
	Genuine *bool
}

func (v verdict) String() string {
	genuine := "null"
	if v.Genuine != nil {
		genuine = strconv.FormatBool(*v.Genuine)
	}
	return fmt.Sprintf("block %d fired [%s] genuine %s", v.Block, v.Fired, genuine)
}

func (v verdict) is(fired string, genuine bool) bool {
	return v.Fired == fired && v.Genuine != nil && *v.Genuine == genuine
}

func verdicts(t *testing.T, d *testkit.DB) []verdict {
	t.Helper()
	rows, err := d.Store.Pool.Query(t.Context(), verdictsSQL)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pgx.CollectRows(rows, pgx.RowToStructByPos[verdict])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func expectAll(t *testing.T, vs []verdict, fired string, genuine bool) {
	t.Helper()
	for _, v := range vs {
		if !v.is(fired, genuine) {
			t.Fatalf("%v, want fired [%s] genuine %v", v, fired, genuine)
		}
	}
}

type facilitatorDay struct {
	RawPayments     int64
	RawVolume       string
	GenuinePayments int64
	GenuineVolume   string
	RawSellers      int64
	RawPayers       int64
}

func facilitator(t *testing.T, d *testkit.DB, day int64, tier, id string) facilitatorDay {
	t.Helper()
	var f facilitatorDay
	err := d.Store.Pool.QueryRow(t.Context(), `select raw_payments, raw_volume::text, genuine_payments, genuine_volume::text, raw_sellers, raw_payers
		from daily_facilitator where day = $1 and tier = $2 and facilitator_id = $3`, day, tier, id).
		Scan(&f.RawPayments, &f.RawVolume, &f.GenuinePayments, &f.GenuineVolume, &f.RawSellers, &f.RawPayers)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func send(block int64, payer, payee, amount string) testkit.Row {
	r := pay(block)
	r.Payer, r.Payee, r.Amount = payer, payee, amount
	return r
}

func viaTestRelayer(r testkit.Row) testkit.Row {
	r.Relayer, r.Method = testRelayer, "permit2_exact"
	return r
}

func mint(block int64, recipient, relayer, amount string) testkit.Mint {
	return testkit.Mint{Block: block, LogIndex: 1, BlockTime: indexerTime, Tx: testkit.Hash(int(block) + 7_000_000), Recipient: recipient, Relayer: relayer, Amount: amount}
}

func mintsTo(first int64, n int, relayer, amount string) []testkit.Mint {
	out := make([]testkit.Mint, n)
	for i := range out {
		out[i] = mint(first+int64(i), testkit.Addr(i), relayer, amount)
	}
	return out
}

func twoMolandaks(t *testing.T) *registry.Registry {
	t.Helper()
	entry := `address: "` + molandak + `"`
	return registryFrom(t, entry, entry+"\n      - address: \""+secondMolandak+`"`)
}

func TestMolandakBurstIsNotGenuine(t *testing.T) {
	d, cfg, reg := setup(t, 5000)
	const day = 20000
	d.Insert(t, testkit.Burst(1000, 1518, testkit.BurstOpts{Payers: 56, Payee: seller, Relayer: molandak, Amount: "100", BlockTime: day*86400 + 100})...)
	d.InsertMints(t, mintsTo(500, 51, minter, "480379")...)
	once(t, d, cfg, reg)
	vs := verdicts(t, d)
	if len(vs) != 1518 {
		t.Fatalf("%d verdicts", len(vs))
	}
	for i, v := range vs {
		fired := "fixed_amount,payee_concentration,no_other_activity"
		if i%56 < 51 {
			fired += ",bridge_funded"
		}
		if !v.is(fired, false) {
			t.Fatalf("%v, want fired [%s] genuine false", v, fired)
		}
	}
	want := facilitatorDay{RawPayments: 1518, RawVolume: "151800", GenuinePayments: 0, GenuineVolume: "0", RawSellers: 1, RawPayers: 56}
	if got := facilitator(t, d, day, "A", "monad-foundation"); got != want {
		t.Fatalf("daily_facilitator %+v, want %+v", got, want)
	}
	checks := []struct {
		name, query string
		want        int64
	}{
		{"seller rows", `select count(*) from daily_seller`, 1},
		{"seller payments", `select sum(raw_payments)::bigint from daily_seller`, 1518},
		{"seller volume", `select sum(raw_volume)::bigint from daily_seller`, 151800},
		{"seller payers", `select sum(raw_payers)::bigint from daily_seller`, 56},
		{"seller genuine", `select sum(genuine_payments)::bigint from daily_seller`, 0},
		{"payer rows", `select count(*) from daily_payer`, 56},
		{"payer payments", `select sum(raw_payments)::bigint from daily_payer`, 1518},
		{"payer volume", `select sum(raw_volume)::bigint from daily_payer`, 151800},
		{"payer sellers", `select sum(raw_sellers)::bigint from daily_payer`, 56},
		{"payer genuine", `select sum(genuine_payments)::bigint from daily_payer`, 0},
	}
	for _, c := range checks {
		if got := scalar[int64](t, d, c.query); got != c.want {
			t.Errorf("%s = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMintClusterNeedsTenRecipients(t *testing.T) {
	flagged := func(t *testing.T, d *testkit.DB) int {
		return scalar[int](t, d, `select count(*) from payer_flag where bridge_funded`)
	}
	t.Run("nine recipients do not flag and the tenth flags all ten", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		d.InsertMints(t, mintsTo(100, 9, minter, "480379")...)
		once(t, d, cfg, reg)
		if n := flagged(t, d); n != 0 {
			t.Fatalf("%d flagged with 9 recipients", n)
		}
		d.InsertMints(t, mint(2000, testkit.Addr(9), minter, "480379"))
		d.SetProgress(t, 2100, indexerTime)
		once(t, d, cfg, reg)
		if n := flagged(t, d); n != 10 {
			t.Fatalf("%d flagged with 10 recipients", n)
		}
	})
	t.Run("one recipient minted 53 times does not flag", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		for i := range 53 {
			d.InsertMints(t, mint(int64(100+i), testkit.Addr(1), minter, "62"))
		}
		once(t, d, cfg, reg)
		if n := flagged(t, d); n != 0 {
			t.Fatalf("%d flagged", n)
		}
	})
	t.Run("two relayers split the cluster", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		d.InsertMints(t, mintsTo(100, 5, minter, "480379")...)
		d.InsertMints(t, mintsTo(200, 5, testkit.Addr(40), "480379")...)
		once(t, d, cfg, reg)
		if n := flagged(t, d); n != 0 {
			t.Fatalf("%d flagged", n)
		}
	})
}

func TestStraysAreGenuine(t *testing.T) {
	d, cfg, reg := setup(t, 111_900_000)
	first := send(111_860_097, testkit.Addr(1), testkit.Addr(2), "1000000")
	first.Relayer = unlisted
	d.Insert(t, first, send(111_860_485, testkit.Addr(3), strayPayee, "10000"), send(111_863_489, testkit.Addr(4), strayPayee, "10000"))
	once(t, d, cfg, reg)
	expectAll(t, verdicts(t, d), "", true)
	if got := column[string](t, d, `select tier from payment order by block`); fmt.Sprint(got) != "[B A A]" {
		t.Fatalf("tiers %v", got)
	}
}

func TestSelfPayment(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, send(100, selfPayer, selfPayer, "1000"))
	once(t, d, cfg, reg)
	vs := verdicts(t, d)
	if len(vs) != 1 || !vs[0].is("self_payment", false) {
		t.Fatalf("verdicts %v", vs)
	}
}

func TestOneSignalIsGenuine(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	rows := make([]testkit.Row, 100)
	for i := range rows {
		rows[i] = send(int64(100+i), testkit.Addr(1000+i), testkit.Addr(2), "1000")
		if i%2 == 1 {
			rows[i].Relayer = unlisted
		}
	}
	d.Insert(t, rows...)
	once(t, d, cfg, reg)
	vs := verdicts(t, d)
	if len(vs) != 100 {
		t.Fatalf("%d verdicts", len(vs))
	}
	expectAll(t, vs, "fixed_amount", true)
}

func TestThresholdBoundaries(t *testing.T) {
	t.Run("fixed_amount", func(t *testing.T) {
		cases := []struct {
			name        string
			rows, alike int
			want        int
		}{
			{"99 rows all alike", 99, 99, 0},
			{"100 rows with 90 alike (share exactly 0.90)", 100, 90, 90},
			{"100 rows with 89 alike", 100, 89, 0},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				d, cfg, reg := setup(t, 1000)
				rows := make([]testkit.Row, c.rows)
				for i := range rows {
					amount := "1000"
					if i >= c.alike {
						amount = strconv.Itoa(2000 + i)
					}
					rows[i] = send(int64(100+i), testkit.Addr(1000+i), testkit.Addr(2), amount)
				}
				d.Insert(t, rows...)
				once(t, d, cfg, reg)
				if got := scalar[int](t, d, `select count(*) from payment_signals where fixed_amount`); got != c.want {
					t.Fatalf("%d rows flagged, want %d", got, c.want)
				}
			})
		}
	})
	t.Run("no_other_activity", func(t *testing.T) {
		payer := testkit.Addr(1000)
		fiveRows := func() []testkit.Row {
			rows := make([]testkit.Row, 5)
			for i := range rows {
				rows[i] = send(int64(100+i), payer, testkit.Addr(2), "1000")
			}
			return rows
		}
		twoPayees := fiveRows()
		twoPayees[4].Payee = testkit.Addr(3)
		asRelayer := send(200, testkit.Addr(5), testkit.Addr(6), "1000")
		asRelayer.Relayer = payer
		cases := []struct {
			name string
			rows []testkit.Row
			want int
		}{
			{"4 rows", fiveRows()[:4], 0},
			{"5 rows", fiveRows(), 5},
			{"5 rows to two payees", twoPayees, 0},
			{"5 rows and a payee in a tier X row", append(fiveRows(), viaTestRelayer(send(200, testkit.Addr(5), payer, "1000"))), 0},
			{"5 rows and a relayer", append(fiveRows(), asRelayer), 0},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				d, cfg, reg := setup(t, 1000)
				d.Insert(t, c.rows...)
				once(t, d, cfg, reg)
				got := scalar[int](t, d, `select count(*) from payment_signals where payer = decode(substr($1, 3), 'hex') and no_other_activity`, payer)
				if got != c.want {
					t.Fatalf("%d rows flagged, want %d", got, c.want)
				}
			})
		}
	})
}

func TestTierXIsNotAggregated(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, viaTestRelayer(pay(100)), pay(101))
	once(t, d, cfg, reg)
	vs := verdicts(t, d)
	if len(vs) != 2 || vs[0].Genuine != nil || vs[0].Fired != "" || !vs[1].is("", true) {
		t.Fatalf("verdicts %v", vs)
	}
	for _, table := range []string{"daily_facilitator", "daily_seller", "daily_payer"} {
		if n := scalar[int](t, d, `select coalesce(sum(raw_payments), 0)::int from `+table); n != 1 {
			t.Errorf("%s counts %d payments, want 1", table, n)
		}
	}
}

func TestTierXFiresNoSignals(t *testing.T) {
	t.Run("a row beside a flagged group", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		rows := make([]testkit.Row, 100)
		for i := range rows {
			rows[i] = send(int64(100+i), testkit.Addr(1000+i), testkit.Addr(2), "1000")
		}
		d.Insert(t, append(rows, viaTestRelayer(send(300, testkit.Addr(2), testkit.Addr(2), "1000")))...)
		once(t, d, cfg, reg)
		vs := verdicts(t, d)
		if last := vs[len(vs)-1]; last.Block != 300 || last.Fired != "" || last.Genuine != nil {
			t.Fatalf("%v", last)
		}
		if n := scalar[int](t, d, `select count(*) from payee_day_flag`); n != 1 {
			t.Fatalf("%d flagged groups", n)
		}
	})
	t.Run("a tier X row does not fill a group", func(t *testing.T) {
		d, cfg, reg := setup(t, 1000)
		rows := make([]testkit.Row, 99)
		for i := range rows {
			rows[i] = send(int64(100+i), testkit.Addr(1000+i), testkit.Addr(2), "1000")
		}
		d.Insert(t, append(rows, viaTestRelayer(send(300, testkit.Addr(5), testkit.Addr(2), "1000")))...)
		once(t, d, cfg, reg)
		if n := scalar[int](t, d, `select count(*) from payee_day_flag`); n != 0 {
			t.Fatalf("%d flagged groups", n)
		}
	})
}

func TestPermit2RowWithNullFieldsIsCountedNotNull(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	bare := func(block int64, relayer string) testkit.Row {
		return testkit.Row{Block: block, LogIndex: 1, BlockTime: indexerTime, Tx: testkit.Hash(int(block)), Method: "permit2_exact", Relayer: relayer}
	}
	d.Insert(t, bare(100, unlisted), bare(101, molandak))
	once(t, d, cfg, reg)
	vs := verdicts(t, d)
	if len(vs) != 2 {
		t.Fatalf("%d verdicts", len(vs))
	}
	expectAll(t, vs, "", true)
	const day = indexerTime / 86400
	want := facilitatorDay{RawPayments: 1, RawVolume: "0", GenuinePayments: 1, GenuineVolume: "0"}
	for _, c := range []struct{ tier, id string }{{"A", "monad-foundation"}, {"B", unlisted}} {
		if got := facilitator(t, d, day, c.tier, c.id); got != want {
			t.Errorf("daily_facilitator %s %s = %+v, want %+v", c.tier, c.id, got, want)
		}
	}
	for _, table := range []string{"daily_seller", "daily_payer"} {
		if n := scalar[int](t, d, `select count(*) from `+table); n != 0 {
			t.Errorf("%s holds %d rows", table, n)
		}
	}
}

func TestVolumeIsUsdcOnly(t *testing.T) {
	d, cfg, reg := setup(t, 1000)
	d.Insert(t, send(100, testkit.Addr(1), testkit.Addr(2), "2000000"))
	once(t, d, cfg, reg)
	const day = indexerTime / 86400
	want := facilitatorDay{RawPayments: 1, RawVolume: "2000000", GenuinePayments: 1, GenuineVolume: "2000000", RawSellers: 1, RawPayers: 1}
	if got := facilitator(t, d, day, "A", "monad-foundation"); got != want {
		t.Fatalf("before the other token %+v, want %+v", got, want)
	}
	odd := send(1001, testkit.Addr(3), testkit.Addr(4), "5000000")
	big := send(1002, testkit.Addr(5), testkit.Addr(6), maxUint256)
	odd.Token, big.Token = otherToken, otherToken
	d.Insert(t, odd, big)
	d.SetProgress(t, 1100, indexerTime)
	once(t, d, cfg, reg)
	want.RawPayments, want.GenuinePayments, want.RawSellers, want.RawPayers = 3, 3, 3, 3
	if got := facilitator(t, d, day, "A", "monad-foundation"); got != want {
		t.Fatalf("after the other token %+v, want %+v", got, want)
	}
}

func TestPayerFlagFlipsOldDays(t *testing.T) {
	d, cfg, _ := setup(t, 300)
	reg := twoMolandaks(t)
	const day = 1
	owner := testkit.Addr(900)
	rows := make([]testkit.Row, 100)
	for i := range rows {
		payer := testkit.Addr(1000 + i)
		if i < 5 {
			payer = owner
		}
		rows[i] = send(int64(100+i), payer, testkit.Addr(50), "1000")
		rows[i].BlockTime = day*86400 + 100
		if i >= 50 {
			rows[i].Relayer = secondMolandak
		}
	}
	d.Insert(t, rows...)
	if res := once(t, d, cfg, reg); res.DirtyDays != 1 {
		t.Fatalf("first refresh rebuilt %d days, want 1", res.DirtyDays)
	}
	vs := verdicts(t, d)
	expectAll(t, vs[:5], "fixed_amount,no_other_activity", false)
	expectAll(t, vs[5:], "fixed_amount", true)
	if got := facilitator(t, d, day, "A", "monad-foundation"); got.RawPayments != 100 || got.GenuinePayments != 95 {
		t.Fatalf("before the flip %+v", got)
	}

	later := send(400, testkit.Addr(7), owner, "5")
	later.BlockTime = (day+1)*86400 + 100
	d.Insert(t, later)
	d.SetProgress(t, 500, indexerTime)
	if res := once(t, d, cfg, reg); res.DirtyDays != 2 {
		t.Fatalf("second refresh rebuilt %d days, want 2", res.DirtyDays)
	}
	expectAll(t, verdicts(t, d)[:5], "fixed_amount", true)
	if got := facilitator(t, d, day, "A", "monad-foundation"); got.RawPayments != 100 || got.GenuinePayments != 100 {
		t.Fatalf("after the flip %+v", got)
	}
}

type loadStep struct {
	rows  []testkit.Row
	mints []testkit.Mint
}

func rebuildSteps() [3]loadStep {
	at := func(r testkit.Row, day int64) testkit.Row {
		r.BlockTime = day*86400 + 100
		return r
	}
	clusterRelayer := testkit.Addr(41)
	owner := testkit.Addr(900)
	var one, two, three loadStep

	one.rows = testkit.Burst(1000, 1518, testkit.BurstOpts{Payers: 56, Payee: seller, Relayer: molandak, Amount: "100", BlockTime: 100*86400 + 100})
	first := send(3000, testkit.Addr(2001), testkit.Addr(2002), "1000000")
	first.Relayer = unlisted
	one.rows = append(one.rows, at(first, 110),
		at(send(3001, testkit.Addr(2003), strayPayee, "10000"), 110),
		at(send(3002, testkit.Addr(2004), strayPayee, "10000"), 110),
		at(send(3100, selfPayer, selfPayer, "1000"), 110))
	for i := range 100 {
		payer := testkit.Addr(1000 + i)
		if i < 5 {
			payer = owner
		}
		r := at(send(int64(4000+i), payer, testkit.Addr(60), "1000"), 1)
		if i >= 50 {
			r.Relayer = secondMolandak
		}
		one.rows = append(one.rows, r)
	}
	for i := range 60 {
		one.rows = append(one.rows, at(send(int64(5000+i), testkit.Addr(5000+i), testkit.Addr(777), "7"), 120))
	}
	for i := range 10 {
		one.rows = append(one.rows, at(send(int64(6000+i), testkit.Addr(300+i), testkit.Addr(778), "55"), 130))
		if i < 6 {
			one.mints = append(one.mints, mint(int64(6100+i), testkit.Addr(300+i), clusterRelayer, "31337"))
		}
	}
	for i := range 5 {
		one.rows = append(one.rows, at(send(int64(7000+i), testkit.Addr(8000), testkit.Addr(779), "9"), 140))
	}
	for i := range 100 {
		amount := "5"
		if i >= 90 {
			amount = strconv.Itoa(100 + i)
		}
		one.rows = append(one.rows, at(send(int64(8000+i), testkit.Addr(6000+i), testkit.Addr(780), amount), 160))
	}

	two.mints = mintsTo(10_000, 51, minter, "480379")
	for i := range 60 {
		two.rows = append(two.rows, at(send(int64(11_000+i), testkit.Addr(5100+i), testkit.Addr(777), "7"), 120))
	}
	two.rows = append(two.rows, at(send(12_000, testkit.Addr(7), owner, "5"), 2))
	for i := range 20 {
		two.rows = append(two.rows, at(send(int64(13_000+i), testkit.Addr(6100+i), testkit.Addr(780), strconv.Itoa(200+i)), 160))
	}

	for i := range 4 {
		three.mints = append(three.mints, mint(int64(20_000+i), testkit.Addr(306+i), clusterRelayer, "31337"))
	}
	for i := range 3 {
		three.mints = append(three.mints, mint(int64(20_010+i), testkit.Addr(52+i), minter, "480379"))
	}
	viaZ := at(send(20_100, testkit.Addr(8001), testkit.Addr(8002), "9"), 150)
	viaZ.Relayer = testkit.Addr(8000)
	three.rows = append(three.rows, viaZ, at(send(20_200, testkit.Addr(1), testkit.Addr(8003), "9"), 150))
	return [3]loadStep{one, two, three}
}

func dump(t *testing.T, d *testkit.DB) []string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`select to_jsonb(s)::text from payment_signals s order by block, log_index`,
		`select to_jsonb(f)::text from payer_flag f order by payer`,
		`select to_jsonb(f)::text from payee_day_flag f order by payee, day`,
		`select to_jsonb(f)::text from relayer_day_flag f order by relayer, day`,
		`select to_jsonb(f)::text from daily_facilitator f order by day, tier, facilitator_id`,
		`select to_jsonb(f)::text from daily_seller f order by day, tier, seller`,
		`select to_jsonb(f)::text from daily_payer f order by day, tier, payer`,
	} {
		out = append(out, column[string](t, d, q)...)
	}
	return out
}

func TestIncrementalRefreshEqualsFullRebuild(t *testing.T) {
	steps := rebuildSteps()
	reg := twoMolandaks(t)
	groupFlagged := `select count(*) from payment_signals where payee = decode(substr($1, 3), 'hex') and fixed_amount`
	ownerGenuine := `select count(*) from payment_signals where payer = decode(substr($1, 3), 'hex') and genuine`
	checks := []struct {
		name, query string
		args        []any
		want        [3]int
	}{
		{"rows flagged in the group that crosses 100 rows", groupFlagged, []any{testkit.Addr(777)}, [3]int{0, 120, 120}},
		{"rows flagged in the group that falls under 0.90", groupFlagged, []any{testkit.Addr(780)}, [3]int{90, 0, 0}},
		{"payers bridge funded", `select count(*) from payer_flag where bridge_funded`, nil, [3]int{0, 51, 64}},
		{"genuine rows of the payer who becomes a payee", ownerGenuine, []any{testkit.Addr(900)}, [3]int{0, 5, 5}},
	}

	inc, incCfg, _ := setup(t, 0)
	progress := [3]int64{9_000, 19_000, 30_000}
	for i, s := range steps {
		inc.Insert(t, s.rows...)
		inc.InsertMints(t, s.mints...)
		inc.SetProgress(t, progress[i], indexerTime)
		once(t, inc, incCfg, reg)
		for _, c := range checks {
			if got := scalar[int](t, inc, c.query, c.args...); got != c.want[i] {
				t.Fatalf("after step %d: %s = %d, want %d", i+1, c.name, got, c.want[i])
			}
		}
	}

	for name, batchRows := range map[string]int{"one batch": 500_000, "many batches": 100} {
		full, fullCfg, _ := setup(t, 30_000)
		fullCfg.IngestBatchRows = batchRows
		for _, s := range steps {
			full.Insert(t, s.rows...)
			full.InsertMints(t, s.mints...)
		}
		once(t, full, fullCfg, reg)
		got, want := dump(t, inc), dump(t, full)
		if len(got) != len(want) || len(got) < 1700 {
			t.Fatalf("%s: the incremental dump holds %d lines, the rebuild %d", name, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: line %d differs\nincremental %s\nrebuild     %s", name, i, got[i], want[i])
			}
		}
	}
}
