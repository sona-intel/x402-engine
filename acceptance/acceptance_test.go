package acceptance

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	chainPath = "/internal/v1/chains/eip155:143/x402"
	usdc      = "754704bc059f8c67012fed69bc8a327a5aafb603"
	molandak  = "7f6a2850669202519f0fe8aa912451238820db86"
	minterA   = "99f5a2e5d81f22ddb130f4a194584532a3dc3c2e"
	minterB   = "c0ac9fdde6e02df3a63d0bbdab28a404332e0ba4"
)

var null = fmt.Sprint(nil)

const firedSQL = `array_remove(array[case when fixed_amount then 'fixed_amount' end, case when payee_concentration then 'payee_concentration' end,
	case when no_other_activity then 'no_other_activity' end, case when bridge_funded then 'bridge_funded' end,
	case when self_payment then 'self_payment' end], null)`

type env struct {
	db  *pgx.Conn
	url string
}

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("X402_ACCEPT_URL")
	if url == "" {
		t.Skip("X402_ACCEPT_URL is not set")
	}
	dsn := os.Getenv("X402_TEST_DSN")
	if dsn == "" {
		t.Skip("X402_TEST_DSN is not set")
	}
	db, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return &env{db: db, url: url}
}

func (e *env) covered(t *testing.T, block int64) {
	t.Helper()
	var watermark int64
	if err := e.db.QueryRow(t.Context(), `select watermark_block from x402.meta where chain_id = 143`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if watermark < block {
		t.Fatalf("the engine watermark %d has not reached block %d", watermark, block)
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) txItems(t *testing.T, tx string) []map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+chainPath+"/payments/"+tx, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Items
}

type windowRow struct {
	USDC    bool
	Tier    string
	Genuine *bool
	Amount  *string
	Fired   []string
}

func (e *env) window(t *testing.T, from, to int64) []windowRow {
	t.Helper()
	rows, err := e.db.Query(t.Context(), `
select coalesce(token = decode($3, 'hex'), false), tier, genuine, amount::text, `+firedSQL+`
from x402.payment_signals where chain_id = 143 and block between $1 and $2`, from, to, usdc)
	if err != nil {
		t.Fatal(err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[windowRow])
	if err != nil {
		t.Fatal(err)
	}
	return all
}

type window struct {
	name     string
	from, to int64
	rows     int
	tiers    map[string]int
	amount   string
	genuine  *bool
	fired    []string
}

var windows = []window{
	{name: "first Molandak call", from: 52_996_000, to: 52_996_999, rows: 1, tiers: map[string]int{"A": 1}},
	{name: "bytes-signature call", from: 61_900_000, to: 61_900_999, rows: 1, tiers: map[string]int{"B": 1}},
	{name: "routed other", from: 76_900_000, to: 76_900_999, rows: 2, tiers: map[string]int{"X": 2}},
	{name: "F2 paced", from: 97_000_000, to: 97_000_999, rows: 169, tiers: map[string]int{"A": 169}, genuine: new(true), fired: []string{"fixed_amount"}},
	{name: "F1 burst 101M", from: 101_000_000, to: 101_000_999, rows: 1656},
	{name: "F1 burst 102.5M", from: 102_500_000, to: 102_500_999, rows: 1615},
	{name: "F1 burst 104.5M", from: 104_500_000, to: 104_500_999, rows: 1554},
	{name: "F1 burst 107M", from: 107_000_000, to: 107_000_999, rows: 1829, tiers: map[string]int{"A": 1828, "X": 1}},
	{name: "F1 burst 108.5M", from: 108_500_000, to: 108_500_999, rows: 1, tiers: map[string]int{"X": 1}},
	{name: "F1 window 111.35M", from: 111_350_000, to: 111_350_999, rows: 1518, tiers: map[string]int{"A": 1518}, amount: "100", genuine: new(false)},
	{name: "F1 window 111.6M", from: 111_600_000, to: 111_600_999, rows: 1528, tiers: map[string]int{"A": 1528}, amount: "100", genuine: new(false)},
	{name: "F1 window 111.7M", from: 111_700_000, to: 111_700_999, rows: 1378, tiers: map[string]int{"A": 1378}, amount: "100", genuine: new(false)},
	{name: "quiet head", from: 112_620_493, to: 112_625_492, rows: 0},
	{name: "strays", from: 111_860_078, to: 111_870_077, rows: 3, tiers: map[string]int{"B": 1, "A": 2}, genuine: new(true)},
	{name: "scripted test F3", from: 112_186_000, to: 112_187_999, rows: 36, genuine: new(true)},
}

func TestGoldenWindows(t *testing.T) {
	e := setup(t)
	for _, w := range windows {
		t.Run(w.name, func(t *testing.T) {
			e.covered(t, w.to)
			var rows []windowRow
			tiers := map[string]int{}
			for _, r := range e.window(t, w.from, w.to) {
				if r.USDC {
					rows = append(rows, r)
					tiers[r.Tier]++
				}
			}
			if len(rows) != w.rows {
				t.Errorf("%d USDC rows, want %d", len(rows), w.rows)
			}
			if w.tiers != nil && !maps.Equal(tiers, w.tiers) {
				t.Errorf("tiers %v, want %v", tiers, w.tiers)
			}
			var badAmount, badGenuine, badFired int
			for _, r := range rows {
				if w.amount != "" && (r.Amount == nil || *r.Amount != w.amount) {
					badAmount++
				}
				if w.genuine != nil && (r.Genuine == nil || *r.Genuine != *w.genuine) {
					badGenuine++
				}
				if w.fired != nil && !slices.Equal(r.Fired, w.fired) {
					badFired++
				}
			}
			if badAmount > 0 {
				t.Errorf("%d rows are not of amount %s", badAmount, w.amount)
			}
			if badGenuine > 0 {
				t.Errorf("%d rows are not genuine = %v", badGenuine, *w.genuine)
			}
			if badFired > 0 {
				t.Errorf("%d rows have rules_fired other than %v", badFired, w.fired)
			}
			t.Logf("USDC rows %d, tiers %v, other tokens %d", len(rows), tiers, len(e.window(t, w.from, w.to))-len(rows))
		})
	}
}

func TestSelfPayments(t *testing.T) {
	e := setup(t)
	for _, block := range []int64{112_301_324, 112_303_231, 112_324_998, 112_325_036, 112_325_051, 112_350_204} {
		t.Run(strconv.FormatInt(block, 10), func(t *testing.T) {
			e.covered(t, block)
			rows, err := e.db.Query(t.Context(), `select genuine, `+firedSQL+` from x402.payment_signals
				where chain_id = 143 and block = $1 and payer = payee`, block)
			if err != nil {
				t.Fatal(err)
			}
			type verdict struct {
				Genuine *bool
				Fired   []string
			}
			got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[verdict])
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("%d self payments, want 1", len(got))
			}
			if got[0].Genuine == nil || *got[0].Genuine {
				t.Errorf("genuine %v, want false", got[0].Genuine)
			}
			if !slices.Contains(got[0].Fired, "self_payment") {
				t.Errorf("rules_fired %v lacks self_payment", got[0].Fired)
			}
		})
	}
}

func TestNonceReconciliation(t *testing.T) {
	e := setup(t)
	for _, r := range []struct {
		from, to int64
		rows     int
	}{{112_186_000, 112_188_000, 36}, {112_188_000, 112_350_500, 7}, {112_350_500, 112_500_000, 2}} {
		t.Run(fmt.Sprintf("%d-%d", r.from, r.to), func(t *testing.T) {
			e.covered(t, r.to)
			n := e.count(t, `select count(*) from x402.payment where chain_id = 143 and relayer = decode($1, 'hex') and block > $2 and block <= $3`,
				molandak, r.from, r.to)
			if n != r.rows {
				t.Errorf("%d Molandak rows, want %d", n, r.rows)
			}
		})
	}
}

func TestBridgeFunded(t *testing.T) {
	e := setup(t)
	e.covered(t, 111_192_999)
	rows, err := e.db.Query(t.Context(), `
select encode(m.relayer, 'hex'), exists (select 1 from x402.payer_flag f where f.chain_id = 143 and f.payer = m.recipient and f.bridge_funded)
from x402.mint m where m.chain_id = 143 and m.block between 111192000 and 111192999 and m.amount = 480379`)
	if err != nil {
		t.Fatal(err)
	}
	type minted struct {
		Relayer string
		Flagged bool
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[minted])
	if err != nil {
		t.Fatal(err)
	}
	total, flagged := map[string]int{}, map[string]int{}
	for _, m := range got {
		total[m.Relayer]++
		if m.Flagged {
			flagged[m.Relayer]++
		}
	}
	if len(got) != 51 || total[minterA] != 3 || total[minterB] != 48 {
		t.Fatalf("mints per relayer %v, want 3 from %s and 48 from %s", total, minterA, minterB)
	}
	if flagged[minterB] != 48 {
		t.Errorf("%d of the 48 recipients minted by %s are bridge_funded", flagged[minterB], minterB)
	}
	if flagged[minterA] != 0 {
		t.Errorf("%d of the 3 recipients minted by %s are bridge_funded, want 0", flagged[minterA], minterA)
	}
}

type txCase struct {
	name     string
	tx       string
	logIndex int
	want     map[string]string
}

var txCases = []txCase{
	{
		name: "first Molandak call", tx: "0xdfab5948beeb75ea2bfa333df9a8a5487a831b7c94cb9c041a9a117ccf55fd25",
		want: map[string]string{
			"method": "eip3009_transfer", "tier": "A", "reason": null, "facilitator": "monad-foundation",
			"gas_cost": "17629595283900000", "block_time": "2026-02-03T16:26:44Z",
		},
	},
	{
		name: "bytes-signature call", tx: "0xa9ffa7a3bdfb867095568c32c8f2bd50af82a65afa0b7e1a4bb108f4715f0a3a",
		want: map[string]string{
			"tier": "B", "reason": null, "facilitator": "0x210f01cf42b4ade5b5f4fa85f483b8a6c31c6f25",
			"selector": "0xcf092995", "amount": "90698",
		},
	},
	{
		name: "Exact settle", tx: "0x3bf7899d2f42e887b5e3814f4455843542329f1adb18d0ae5a71ea894680a862",
		want: map[string]string{
			"method": "permit2_exact", "tier": "X", "reason": "test_relayer", "facilitator": null,
			"payer": "0xd156f8adf8ada32470282b4fc6ba0ddd36c7352d", "payee": "0x1c47e9c085c2b7458f5b6c16ccbd65a65255a9f6",
			"amount": "1000", "valid_after": "1791209657", "genuine": null,
		},
	},
	{
		name: "Exact settleWithPermit", tx: "0x99e40de8ac2932dbbf52609f3f94383bc4954acb03556cc97b861ddb8a8e327a",
		want: map[string]string{
			"method": "permit2_exact", "payer": "0x9412787471f58e3d9a302ede90e8705464ce2bd4", "valid_after": "1791209653",
		},
	},
	{
		name: "Upto settle", tx: "0x7322c6bac7a63a6b11a7bcd6e175202b6ab1f6d76f270037d29a1a436e94f3a1",
		want: map[string]string{"method": "permit2_upto", "amount": "1000", "valid_before": "1791213881"},
	},
	{
		name: "Molandak Exact settle", tx: "0xd86bddd68066957d080afd17e39dd0eeaecfe51bec3acde9a1590b008d1e3ba4",
		want: map[string]string{
			"method": "permit2_exact", "tier": "A", "reason": null, "facilitator": "monad-foundation", "amount": "50000",
		},
	},
	{
		name: "batch deposit", tx: "0x20ae381947f0c42b3b33ad4541dd6d91a3b115d2a6a7c633bbeef736ab07424c",
		want: map[string]string{
			"method": "batch_deposit", "tier": "X", "reason": "not_x402", "facilitator": null, "payee": null, "amount": null,
		},
	},
	{
		name: "Across", tx: "0x33ac641e1d35014972b7c1b378dc5f36473a4314b31ea3368d38337984993e05", logIndex: 110,
		want: map[string]string{
			"method": "other", "tier": "X", "reason": "not_x402", "facilitator": null, "log_index": "110",
			"tx_to": "0x998d7c178a1f9607b47ea3a8e22426451b3ce707", "gas_cost": "56468070400000000",
		},
	},
}

func TestGoldenTransactions(t *testing.T) {
	e := setup(t)
	for _, c := range txCases {
		t.Run(c.name, func(t *testing.T) {
			items := e.txItems(t, c.tx)
			item := items[0]
			if c.logIndex != 0 {
				i := slices.IndexFunc(items, func(m map[string]any) bool { return fmt.Sprint(m["log_index"]) == strconv.Itoa(c.logIndex) })
				if i < 0 {
					t.Fatalf("no row with log_index %d among %d", c.logIndex, len(items))
				}
				item = items[i]
			} else if len(items) != 1 {
				t.Fatalf("%d rows, want 1", len(items))
			}
			for field, want := range c.want {
				if got := fmt.Sprint(item[field]); got != want {
					t.Errorf("%s is %s, want %s", field, got, want)
				}
			}
		})
	}
}
