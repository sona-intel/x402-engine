package testkit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"x402/internal/migrate"
	"x402/internal/store"
)

const USDC = "0x754704bc059f8c67012fed69bc8a327a5aafb603"

type DB struct {
	Store  *store.Store
	Source string
	Engine string
	Chain  int
	codes  bool
}

type Row struct {
	Block       int64
	LogIndex    int
	BlockTime   int64
	Tx          string
	Method      string
	Token       string
	Payer       string
	Payee       string
	Nonce       string
	Relayer     string
	TxTo        string
	Selector    string
	Amount      string
	ValidAfter  string
	ValidBefore string
	GasCost     string
	Codes       string
}

type Mint struct {
	Block     int64
	LogIndex  int
	BlockTime int64
	Tx        string
	Recipient string
	Relayer   string
	TxTo      string
	Amount    string
}

type BurstOpts struct {
	Payers    int
	Payee     string
	Relayer   string
	Token     string
	Amount    string
	BlockTime int64
}

const sourceDDL = `
create schema {src};
create table {src}."X402Payment" (
	id text not null, "blockNumber" integer not null, "blockTime" integer not null, "txHash" text not null, "logIndex" integer not null,
	method text not null, token text, payer text, payee text, amount numeric, nonce text, "validAfter" numeric, "validBefore" numeric,
	relayer text, "txTo" text, selector text, "gasCost" numeric, {codesCol} "chainId" integer not null, primary key (id, "chainId"));
create table {src}."UsdcMint" (
	id text not null, "blockNumber" integer not null, "blockTime" integer not null, "txHash" text not null, recipient text not null,
	amount numeric not null, relayer text, "txTo" text, "chainId" integer not null, primary key (id, "chainId"));
create table {src}."_meta" (
	"chainId" integer primary key, ecosystem text, "startBlock" integer, "endBlock" integer, "progressBlock" integer,
	"progressBlockTime" timestamptz, "bufferBlock" integer, "firstEventBlock" integer, "eventsProcessed" real, "sourceBlock" integer,
	"readyAt" timestamptz, "isReady" boolean);`

const insertPayments = `
insert into {src}."X402Payment" (id, "blockNumber", "blockTime", "txHash", "logIndex", method, token, payer, payee, amount, nonce,
	"validAfter", "validBefore", relayer, "txTo", selector, "gasCost", {codesName} "chainId")
select '{chain}_' || "Block" || '_' || "LogIndex", "Block", "BlockTime", "Tx", "LogIndex", "Method", nullif("Token", ''), nullif("Payer", ''),
	nullif("Payee", ''), nullif("Amount", '')::numeric, nullif("Nonce", ''), nullif("ValidAfter", '')::numeric,
	nullif("ValidBefore", '')::numeric, nullif("Relayer", ''), nullif("TxTo", ''), nullif("Selector", ''), nullif("GasCost", '')::numeric, {codesValue} {chain}
from jsonb_to_recordset($1::jsonb) as r("Block" bigint, "LogIndex" int, "BlockTime" bigint, "Tx" text, "Method" text, "Token" text,
	"Payer" text, "Payee" text, "Nonce" text, "Relayer" text, "TxTo" text, "Selector" text, "Amount" text, "ValidAfter" text,
	"ValidBefore" text, "GasCost" text, "Codes" text)`

const insertMints = `
insert into {src}."UsdcMint" (id, "blockNumber", "blockTime", "txHash", recipient, amount, relayer, "txTo", "chainId")
select '{chain}_' || "Block" || '_' || "LogIndex", "Block", "BlockTime", "Tx", "Recipient", "Amount"::numeric, nullif("Relayer", ''),
	nullif("TxTo", ''), {chain}
from jsonb_to_recordset($1::jsonb) as r("Block" bigint, "LogIndex" int, "BlockTime" bigint, "Tx" text, "Recipient" text,
	"Relayer" text, "TxTo" text, "Amount" text)`

const setProgress = `
insert into {src}."_meta" ("chainId", "progressBlock", "progressBlockTime", "sourceBlock", "isReady")
values ({chain}, $1::int, to_timestamp($2::bigint), $1::int + 3, true)
on conflict ("chainId") do update set "progressBlock" = excluded."progressBlock",
	"progressBlockTime" = excluded."progressBlockTime", "sourceBlock" = excluded."sourceBlock", "isReady" = true`

func New(t testing.TB) *DB {
	t.Helper()
	dsn := os.Getenv("X402_TEST_DSN")
	if dsn == "" {
		t.Skip("X402_TEST_DSN is not set")
	}
	d := &DB{Source: schemaName("src"), Engine: schemaName("eng"), Chain: 143}
	st, err := store.Connect(t.Context(), dsn, d.Engine)
	if err != nil {
		t.Fatal(err)
	}
	d.Store = st
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, s := range []string{d.Source, d.Engine} {
			if _, err := st.Pool.Exec(ctx, "drop schema if exists "+pgx.Identifier{s}.Sanitize()+" cascade"); err != nil {
				t.Errorf("drop schema %s: %v", s, err)
			}
		}
		st.Pool.Close()
	})
	if _, err := st.Pool.Exec(t.Context(), "create schema "+pgx.Identifier{d.Engine}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	d.createSource(t, false)
	return d
}

func (d *DB) Other(t testing.TB, chain int, builderCodes bool) *DB {
	t.Helper()
	o := &DB{Store: d.Store, Source: schemaName("src"), Engine: d.Engine, Chain: chain}
	o.createSource(t, builderCodes)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.Store.Pool.Exec(ctx, "drop schema if exists "+pgx.Identifier{o.Source}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema %s: %v", o.Source, err)
		}
	})
	return o
}

func (d *DB) createSource(t testing.TB, builderCodes bool) {
	t.Helper()
	ddl := strings.ReplaceAll(sourceDDL, "{codesCol}", "")
	if builderCodes {
		ddl = strings.ReplaceAll(sourceDDL, "{codesCol}", `"builderCodes" text,`)
	}
	if _, err := d.Store.Pool.Exec(t.Context(), store.SQL(d.Source, ddl)); err != nil {
		t.Fatal(err)
	}
	d.codes = builderCodes
}

func (d *DB) SQL(q string) string {
	q = strings.ReplaceAll(q, "{chain}", strconv.Itoa(d.Chain))
	if d.codes {
		q = strings.ReplaceAll(strings.ReplaceAll(q, "{codesName}", `"builderCodes",`), "{codesValue}", `nullif("Codes", ''),`)
	}
	q = strings.ReplaceAll(strings.ReplaceAll(q, "{codesName}", ""), "{codesValue}", "")
	return store.SQL(d.Source, q)
}

func schemaName(prefix string) string {
	return prefix + "_" + strconv.FormatUint(rand.Uint64(), 16)
}

func (d *DB) Migrate(t testing.TB) {
	t.Helper()
	if err := migrate.Up(t.Context(), d.Store.Pool); err != nil {
		t.Fatal(err)
	}
}

func (d *DB) Insert(t testing.TB, rows ...Row) {
	t.Helper()
	d.load(t, insertPayments, rows)
}

func (d *DB) InsertMints(t testing.TB, m ...Mint) {
	t.Helper()
	d.load(t, insertMints, m)
}

func (d *DB) load(t testing.TB, q string, rows any) {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.Pool.Exec(t.Context(), d.SQL(q), b); err != nil {
		t.Fatal(err)
	}
}

func (d *DB) SetProgress(t testing.TB, block, blockTime int64) {
	t.Helper()
	if _, err := d.Store.Pool.Exec(t.Context(), d.SQL(setProgress), block, blockTime); err != nil {
		t.Fatal(err)
	}
}

func (d *DB) Reset(t testing.TB) {
	t.Helper()
	if _, err := d.Store.Pool.Exec(t.Context(), d.SQL(`truncate {src}."X402Payment", {src}."UsdcMint"`)); err != nil {
		t.Fatal(err)
	}
}

func Hash(n int) string {
	return digest("hash", int64(n))
}

func Addr(n int) string {
	return digest("addr", int64(n))[:42]
}

func digest(kind string, n int64) string {
	sum := sha256.Sum256([]byte(kind + strconv.FormatInt(n, 10)))
	return "0x" + hex.EncodeToString(sum[:])
}

func Burst(firstBlock int64, n int, o BurstOpts) []Row {
	token := cmp.Or(o.Token, USDC)
	payers := max(o.Payers, 1)
	rows := make([]Row, n)
	for i := range rows {
		block := firstBlock + int64(i)
		rows[i] = Row{
			Block:     block,
			LogIndex:  1,
			BlockTime: o.BlockTime + int64(i),
			Tx:        digest("tx", block),
			Method:    "eip3009_transfer",
			Token:     token,
			Payer:     Addr(i % payers),
			Payee:     o.Payee,
			Nonce:     digest("nonce", block),
			Relayer:   o.Relayer,
			TxTo:      token,
			Selector:  "0xe3ee160e",
			Amount:    o.Amount,
		}
	}
	return rows
}
