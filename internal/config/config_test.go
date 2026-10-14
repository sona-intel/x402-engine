package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func check[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestSeedConfig(t *testing.T) {
	cs, err := Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	check(t, "chains", len(cs), 3)
	c := cs[0]
	check(t, "Chain", c.Chain, "eip155:143")
	check(t, "ChainID", c.ChainID, 143)
	check(t, "RefreshInterval", c.RefreshInterval, 30*time.Second)
	check(t, "MarginBlocks", c.MarginBlocks, 10)
	check(t, "SourceSchema", c.SourceSchema, "envio")
	check(t, "IngestBatchRows", c.IngestBatchRows, 500000)
	check(t, "Rules.Version", c.Rules.Version, "2026-10-12.1")
	check(t, "Rules.VolumeToken", c.Rules.VolumeToken, "0x754704bc059f8c67012fed69bc8a327a5aafb603")
	check(t, "Rules.FixedAmount", c.Rules.FixedAmount, Share{MinRows: 100, Share: 0.90})
	check(t, "Rules.PayeeConcentration", c.Rules.PayeeConcentration, Share{MinRows: 100, Share: 0.90})
	check(t, "Rules.NoOtherActivity", c.Rules.NoOtherActivity, MinRows{MinRows: 5})
	check(t, "Rules.BridgeFunded", c.Rules.BridgeFunded, Cluster{MinRecipients: 10})
	check(t, "BuilderCodes", c.BuilderCodes, false)
	check(t, "RetentionDays", c.RetentionDays, 0)
	check(t, "HistoryFrom", c.HistoryFrom, "")
}

func TestSeedConfigBase(t *testing.T) {
	cs, err := Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	b := cs[1]
	check(t, "Chain", b.Chain, "eip155:8453")
	check(t, "ChainID", b.ChainID, 8453)
	check(t, "SourceSchema", b.SourceSchema, "envio_base")
	check(t, "Rules.VolumeToken", b.Rules.VolumeToken, "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913")
	check(t, "BuilderCodes", b.BuilderCodes, true)
	check(t, "RetentionDays", b.RetentionDays, 90)
	check(t, "HistoryFrom", b.HistoryFrom, "2026-09-13")
	check(t, "RefreshInterval", b.RefreshInterval, cs[0].RefreshInterval)
	if b.Rules.Version != cs[0].Rules.Version || b.Rules.FixedAmount != cs[0].Rules.FixedAmount {
		t.Errorf("Base rules %+v differ from Monad rules %+v", b.Rules, cs[0].Rules)
	}
}

func TestSeedConfigArc(t *testing.T) {
	cs, err := Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	a := cs[2]
	check(t, "Chain", a.Chain, "eip155:5042")
	check(t, "ChainID", a.ChainID, 5042)
	check(t, "SourceSchema", a.SourceSchema, "envio_arc")
	check(t, "Rules.VolumeToken", a.Rules.VolumeToken, "0x3600000000000000000000000000000000000000")
	check(t, "BuilderCodes", a.BuilderCodes, false)
	check(t, "RetentionDays", a.RetentionDays, 0)
	check(t, "HistoryFrom", a.HistoryFrom, "")
	check(t, "GatewayWallet", a.GatewayWallet, "0x77777777dcc4d5a8b6e418fd04d8997ef11000ee")
	check(t, "Rules.Version", a.Rules.Version, cs[0].Rules.Version)
}

// the Monad rules checksum is stored in the live engine, so a change would rebuild its copy
func TestMonadRulesChecksumIsTheOneBeforeBase(t *testing.T) {
	cs, err := Load("../../config")
	if err != nil {
		t.Fatal(err)
	}
	const before = "21e3a83979d20effe8669d39eb408d0eb206b5e0ca4c34c6aa8be7eb379495ef"
	if got := cs[0].Rules.Checksum(); got != before {
		t.Fatalf("Monad rules checksum %s, want %s", got, before)
	}
	if cs[0].Rules.Checksum() == cs[1].Rules.Checksum() {
		t.Fatal("two volume tokens share one rules checksum")
	}
}

const (
	head = "chains:\n  - {id: eip155:143, source_schema: envio, volume_token: \"0x754704bc059f8c67012fed69bc8a327a5aafb603\"}\nrefresh_interval: 30s\nmargin_blocks: 10\ningest_batch_rows: 500000\n"

	rulesPlain = `rules:
  version: 2026-10-12.1
  fixed_amount: {min_rows: 100, share: 0.90}
  payee_concentration: {min_rows: 100, share: 0.90}
  no_other_activity: {min_rows: 5}
  bridge_funded: {min_recipients: 10}
`

	rulesShuffled = `# thresholds
rules:
  bridge_funded:
    min_recipients: 10 # clusters
  no_other_activity:
    min_rows: 5
  payee_concentration:
    share: 0.9
    min_rows: 100
  fixed_amount:
    share: 0.900
    min_rows: 100
  version: "2026-10-12.1"
`
)

func writeDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "engine.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func loadRules(t *testing.T, body string) Rules {
	t.Helper()
	cs, err := Load(writeDir(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return cs[0].Rules
}

func TestChecksumIgnoresFormatting(t *testing.T) {
	plain := loadRules(t, head+rulesPlain)
	if len(plain.Checksum()) != 64 {
		t.Fatalf("checksum %q is not sha256 hex", plain.Checksum())
	}
	if got := loadRules(t, head+rulesShuffled); got.Checksum() != plain.Checksum() {
		t.Fatalf("formatting changed the checksum: %s != %s", got.Checksum(), plain.Checksum())
	}
	bumped := strings.Replace(rulesPlain, "fixed_amount: {min_rows: 100", "fixed_amount: {min_rows: 101", 1)
	if got := loadRules(t, head+bumped); got.Checksum() == plain.Checksum() {
		t.Fatal("a changed threshold kept the checksum")
	}
}

func TestLoadRejects(t *testing.T) {
	chain := func(extra string) string {
		return strings.Replace(head, "volume_token:", extra+", volume_token:", 1)
	}
	cases := map[string]string{
		"another namespace":         strings.Replace(head, "eip155:143", "solana:1", 1) + rulesPlain,
		"chain without a number":    strings.Replace(head, "eip155:143", "eip155:", 1) + rulesPlain,
		"chain with a zero":         strings.Replace(head, "eip155:143", "eip155:0", 1) + rulesPlain,
		"the same chain twice":      strings.Replace(head, "refresh_interval", "  - {id: eip155:143, source_schema: other, volume_token: \"0x754704bc059f8c67012fed69bc8a327a5aafb603\"}\nrefresh_interval", 1) + rulesPlain,
		"one schema twice":          strings.Replace(head, "refresh_interval", "  - {id: eip155:8453, source_schema: envio, volume_token: \"0x754704bc059f8c67012fed69bc8a327a5aafb603\"}\nrefresh_interval", 1) + rulesPlain,
		"no chains":                 strings.Replace(head, head[:strings.Index(head, "refresh_interval")], "", 1) + rulesPlain,
		"empty chain list":          strings.Replace(head, head[:strings.Index(head, "refresh_interval")], "chains: []\n", 1) + rulesPlain,
		"unknown key":               head + "cutoff: 3\n" + rulesPlain,
		"unknown chain key":         chain("cutoff: 3") + rulesPlain,
		"unknown rules key":         head + rulesPlain + "  fixed_amnt: {min_rows: 1, share: 0.5}\n",
		"volume token in the rules": head + rulesPlain + "  volume_token: \"0x754704bc059f8c67012fed69bc8a327a5aafb603\"\n",
		"interval not a duration":   strings.Replace(head, "30s", "soon", 1) + rulesPlain,
		"interval is a bare number": strings.Replace(head, "30s", "30", 1) + rulesPlain,
		"zero interval":             strings.Replace(head, "30s", "0s", 1) + rulesPlain,
		"zero batch":                strings.Replace(head, "500000", "0", 1) + rulesPlain,
		"negative margin":           strings.Replace(head, "margin_blocks: 10", "margin_blocks: -1", 1) + rulesPlain,
		"schema is not a name":      strings.Replace(head, "envio", `"envio; drop"`, 1) + rulesPlain,
		"negative retention":        chain("retention_days: -1") + rulesPlain,
		"history day is not a day":  chain("history_from: 2026-13-45") + rulesPlain,
		"gateway is not an address": chain("gateway_wallet: 0x7777") + rulesPlain,
		"not yaml":                  "chains: [",
		"no rules block":            head,
		"empty rules block":         head + "rules: {}\n",
		"zero fixed_amount rows":    head + strings.Replace(rulesPlain, "fixed_amount: {min_rows: 100", "fixed_amount: {min_rows: 0", 1),
		"payee share above one":     head + strings.Replace(rulesPlain, "payee_concentration: {min_rows: 100, share: 0.90}", "payee_concentration: {min_rows: 100, share: 90}", 1),
		"fixed share at one half":   head + strings.Replace(rulesPlain, "fixed_amount: {min_rows: 100, share: 0.90}", "fixed_amount: {min_rows: 100, share: 0.5}", 1),
		"payee share at one half":   head + strings.Replace(rulesPlain, "payee_concentration: {min_rows: 100, share: 0.90}", "payee_concentration: {min_rows: 100, share: 0.5}", 1),
		"empty volume token":        strings.Replace(head, "0x754704bc059f8c67012fed69bc8a327a5aafb603", "", 1) + rulesPlain,
		"zero bridge cluster":       head + strings.Replace(rulesPlain, "min_recipients: 10", "min_recipients: 0", 1),
	}
	for name, body := range cases {
		if c, err := Load(writeDir(t, body)); err == nil {
			t.Errorf("%s accepted: %+v", name, c)
		}
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("missing engine.yaml accepted")
	}
}
