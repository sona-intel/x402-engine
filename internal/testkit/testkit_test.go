package testkit_test

import (
	"regexp"
	"testing"

	"x402/internal/testkit"
)

var hash = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

func TestHashIsDeterministicAndDistinct(t *testing.T) {
	a, again, b := testkit.Hash(7), testkit.Hash(7), testkit.Hash(8)
	if !hash.MatchString(a) || a != again || a == b {
		t.Fatalf("Hash(7) = %q then %q, Hash(8) = %q", a, again, b)
	}
}

func TestBurstShape(t *testing.T) {
	rows := testkit.Burst(1000, 6, testkit.BurstOpts{Payers: 3, Payee: "0xaa", Relayer: "0xbb", Amount: "100", BlockTime: 5000})
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rows))
	}
	seen := map[string]bool{}
	for i, r := range rows {
		if r.Block != int64(1000+i) || r.LogIndex != 1 || r.BlockTime != int64(5000+i) || r.Method != "eip3009_transfer" || r.Token != testkit.USDC ||
			r.Payee != "0xaa" || r.Relayer != "0xbb" || r.Amount != "100" || r.Payer != rows[i%3].Payer {
			t.Fatalf("row %d = %+v", i, r)
		}
		if seen[r.Tx] || seen[r.Nonce] || r.Tx == r.Nonce {
			t.Fatalf("row %d repeats a tx hash or nonce", i)
		}
		seen[r.Tx], seen[r.Nonce] = true, true
	}
	if rows[0].Payer == rows[1].Payer || rows[0].Payer != rows[3].Payer {
		t.Fatalf("payers do not rotate over 3 addresses")
	}
}

func TestBurstKeepsAnExplicitToken(t *testing.T) {
	rows := testkit.Burst(1, 1, testkit.BurstOpts{Payers: 1, Token: "0xcc"})
	if rows[0].Token != "0xcc" || rows[0].TxTo != "0xcc" {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestInsertStoresAnEmptyStringAsNull(t *testing.T) {
	d := testkit.New(t)
	d.Insert(t, testkit.Row{Block: 10, LogIndex: 2, BlockTime: 99, Tx: testkit.Hash(1), Method: "other", Amount: "1000"})
	var id, token *string
	var amount, gas *string
	err := d.Store.Pool.QueryRow(t.Context(), d.SQL(`select id, token, amount::text, "gasCost"::text from {src}."X402Payment"`)).Scan(&id, &token, &amount, &gas)
	if err != nil {
		t.Fatal(err)
	}
	if *id != "143_10_2" || token != nil || *amount != "1000" || gas != nil {
		t.Fatalf("id %v token %v amount %v gas %v", *id, token, amount, gas)
	}
}

func TestInsertMintsBuildsTheID(t *testing.T) {
	d := testkit.New(t)
	d.InsertMints(t, testkit.Mint{Block: 11, LogIndex: 3, BlockTime: 100, Tx: testkit.Hash(2), Recipient: "0xaa", Amount: "480379"})
	var id string
	var relayer *string
	if err := d.Store.Pool.QueryRow(t.Context(), d.SQL(`select id, relayer from {src}."UsdcMint"`)).Scan(&id, &relayer); err != nil {
		t.Fatal(err)
	}
	if id != "143_11_3" || relayer != nil {
		t.Fatalf("id %q relayer %v", id, relayer)
	}
}

func TestSetProgressWritesTheMetaRow(t *testing.T) {
	d := testkit.New(t)
	d.SetProgress(t, 500, 1770136004)
	d.SetProgress(t, 600, 1770136104)
	var progress, source int
	var ready bool
	var epoch int64
	err := d.Store.Pool.QueryRow(t.Context(), d.SQL(`select "progressBlock", "sourceBlock", "isReady", extract(epoch from "progressBlockTime")::bigint from {src}."_meta"`)).
		Scan(&progress, &source, &ready, &epoch)
	if err != nil {
		t.Fatal(err)
	}
	if progress != 600 || source != 603 || !ready || epoch != 1770136104 {
		t.Fatalf("progress %d source %d ready %v time %d", progress, source, ready, epoch)
	}
}

func TestResetEmptiesTheSourceTablesOnly(t *testing.T) {
	d := testkit.New(t)
	d.Insert(t, testkit.Row{Block: 1, LogIndex: 1, BlockTime: 1, Tx: testkit.Hash(1), Method: "other"})
	d.InsertMints(t, testkit.Mint{Block: 1, LogIndex: 2, BlockTime: 1, Tx: testkit.Hash(1), Recipient: "0xaa", Amount: "1"})
	d.SetProgress(t, 1, 1)
	d.Reset(t)
	var payments, mints, meta int
	err := d.Store.Pool.QueryRow(t.Context(), d.SQL(`select (select count(*) from {src}."X402Payment"), (select count(*) from {src}."UsdcMint"), (select count(*) from {src}."_meta")`)).
		Scan(&payments, &mints, &meta)
	if err != nil {
		t.Fatal(err)
	}
	if payments != 0 || mints != 0 || meta != 1 {
		t.Fatalf("payments %d mints %d meta rows %d", payments, mints, meta)
	}
}

func TestOtherChainWritesItsOwnSchemaWithBuilderCodes(t *testing.T) {
	d := testkit.New(t)
	b := d.Other(t, 8453, true)
	b.Insert(t, testkit.Row{Block: 7, LogIndex: 2, BlockTime: 1790000000, Tx: testkit.Hash(7), Method: "eip3009_transfer", Codes: "cdp_facil1"})
	var id, codes string
	if err := b.Store.Pool.QueryRow(t.Context(), b.SQL(`select id, "builderCodes" from {src}."X402Payment"`)).Scan(&id, &codes); err != nil {
		t.Fatal(err)
	}
	if id != "8453_7_2" || codes != "cdp_facil1" {
		t.Fatalf("row %s %s", id, codes)
	}
	var n int
	if err := d.Store.Pool.QueryRow(t.Context(), d.SQL(`select count(*) from {src}."X402Payment"`)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the first chain's schema holds %d rows, err %v", n, err)
	}
}
