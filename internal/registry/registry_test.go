package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSeedRegistry(t *testing.T) {
	r, err := Load("../../config/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "2026-10-12.1" || len(r.Checksum) != 64 {
		t.Fatal(r.Version, r.Checksum)
	}
	ids := make([]string, len(r.Entries))
	counts := make([]int, len(r.Entries))
	for i, e := range r.Entries {
		ids[i], counts[i] = e.ID, len(e.Addresses)
	}
	if got := strings.Join(ids, ","); got != "monad-foundation,openx402,dexter,pieverse,unlisted,test" {
		t.Fatal(got)
	}
	want := []int{1, 1, 1, 2, 5, 2}
	total := 0
	for i, n := range counts {
		total += n
		if n != want[i] {
			t.Errorf("%s has %d addresses, want %d", ids[i], n, want[i])
		}
	}
	if total != 12 {
		t.Errorf("%d addresses, want 12", total)
	}
	for _, e := range r.Entries {
		for _, a := range e.Addresses {
			if a.Hex != strings.ToLower(a.Hex) {
				t.Errorf("%s: %s is not lowercase", e.ID, a.Hex)
			}
		}
		if (e.Class == "facilitator") != (e.URL != "") || e.Name == "" || (e.Class != "facilitator" && e.Class != e.ID) {
			t.Errorf("entry shape: %+v", e)
		}
	}
}

const base = `version: 2026-10-12.1
entries:
  - id: one
    name: One
    url: https://one.example
    class: facilitator
    addresses:
      - address: "0x00000000000000000000000000000000000000aa"
        from_block: 5
`

func TestChecksum(t *testing.T) {
	plain, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	shuffled, err := Load(write(t, `# registry
entries:
  - class: facilitator # kind
    url: https://one.example
    addresses:
      - from_block: 5
        address: "0x00000000000000000000000000000000000000AA"
    name: One
    id: one
version: "2026-10-12.1"
`))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Checksum != shuffled.Checksum {
		t.Fatalf("formatting changed the checksum: %s != %s", plain.Checksum, shuffled.Checksum)
	}
	other, err := Load(write(t, strings.Replace(base, "aa\"", "ab\"", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if other.Checksum == plain.Checksum {
		t.Fatal("a changed address kept the checksum")
	}
	bumped, err := Load(write(t, strings.Replace(base, "from_block: 5", "from_block: 6", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if bumped.Checksum == plain.Checksum {
		t.Fatal("a changed range kept the checksum")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"short address":       strings.Replace(base, "0x00000000000000000000000000000000000000aa", "0x00aa", 1),
		"long address":        strings.Replace(base, "aa\"", "aa00\"", 1),
		"no 0x prefix":        strings.Replace(base, "0x00000000000000000000000000000000000000aa", "00000000000000000000000000000000000000aa", 1),
		"non hex address":     strings.Replace(base, "aa\"", "zz\"", 1),
		"unknown class":       strings.Replace(base, "class: facilitator", "class: relayer", 1),
		"missing class":       strings.Replace(base, "    class: facilitator\n", "", 1),
		"version without n":   strings.Replace(base, "2026-10-12.1", "2026-10-12", 1),
		"version with prefix": strings.Replace(base, "2026-10-12.1", "v2026-10-12.1", 1),
		"version not a date":  strings.Replace(base, "2026-10-12.1", "12-10-2026.1", 1),
		"missing version":     strings.Replace(base, "version: 2026-10-12.1\n", "", 1),
		"duplicate address":   base + "      - {address: \"0x00000000000000000000000000000000000000AA\", from_block: 5}\n",
		"duplicate open from": strings.Replace(base, "        from_block: 5\n", "", 1) + "      - {address: \"0x00000000000000000000000000000000000000aa\", from_block: 0}\n",
		"inverted range":      base + "      - {address: \"0x00000000000000000000000000000000000000ab\", from_block: 9, to_block: 8}\n",
		"unknown key":         base + "      - {address: \"0x00000000000000000000000000000000000000ab\", from_blok: 1}\n",
		"not yaml":            "entries: [",
	}
	for name, body := range cases {
		if r, err := Load(write(t, body)); err == nil {
			t.Errorf("%s accepted: %+v", name, r)
		}
	}
}

func TestLoadAcceptsSuccessiveRanges(t *testing.T) {
	body := base + "      - {address: \"0x00000000000000000000000000000000000000AA\", from_block: 0, to_block: 4}\n"
	if _, err := Load(write(t, body)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

// the Monad checksum is stored in the live engine, so a change would rebuild its copy
func TestMonadChecksumIsTheOneBeforeBase(t *testing.T) {
	r, err := Load("../../config/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const before = "123a6e6c0d5498edb63b4af92319dd147a5b64b5956ec4e522089cd68f74ad19"
	if r.Checksum != before {
		t.Fatalf("Monad registry checksum %s, want %s", r.Checksum, before)
	}
}

func TestSeedRegistryBase(t *testing.T) {
	root, err := Load("../../config/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := root.For("eip155:8453")
	if !ok {
		t.Fatal("no Base registry")
	}
	if r.Version != "2026-10-13.2" || len(r.Checksum) != 64 || r.Checksum == root.Checksum {
		t.Fatal(r.Version, r.Checksum)
	}
	if len(r.Entries) != 1 {
		t.Fatalf("%d Base entries, want 1", len(r.Entries))
	}
	e := r.Entries[0]
	if e.ID != "coinbase-cdp" || e.Class != "facilitator" || len(e.Addresses) != 20 || len(e.Codes) != 1 || e.Codes[0] != "cdp_facil1" {
		t.Fatalf("Base entry %+v", e)
	}
	codes, ids := r.Codes()
	if len(codes) != 1 || codes[0] != "cdp_facil1" || ids[0] != "coinbase-cdp" {
		t.Fatalf("codes %v ids %v", codes, ids)
	}
}

func TestForReturnsTheRootForMonadAndNothingForAnUnknownChain(t *testing.T) {
	root, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := root.For(RootChain); !ok || got != root {
		t.Fatal("the root chain is not the top level")
	}
	if _, ok := root.For("eip155:8453"); ok {
		t.Fatal("a chain the file does not list was found")
	}
}

func TestAChainRegistryDoesNotMoveTheRootChecksum(t *testing.T) {
	plain, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	with, err := Load(write(t, base+"chains:\n  eip155:8453:\n    version: 2026-10-13.1\n    entries: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if with.Checksum != plain.Checksum {
		t.Fatalf("adding a chain moved the root checksum: %s != %s", with.Checksum, plain.Checksum)
	}
	b, _ := with.For("eip155:8453")
	if b.Checksum == "" || b.Checksum == with.Checksum {
		t.Fatalf("Base checksum %q", b.Checksum)
	}
}

func TestLoadRejectsBadChainsAndCodes(t *testing.T) {
	chain := "chains:\n  eip155:8453:\n    version: 2026-10-13.1\n    entries:\n"
	cdp := "      - {id: cdp, name: CDP, class: facilitator, builder_codes: [cdp_facil1]}\n"
	cases := map[string]string{
		"the root chain again":       base + "chains:\n  eip155:143:\n    version: 2026-10-13.1\n    entries: []\n",
		"a chain that is not eip155": base + "chains:\n  solana:1:\n    version: 2026-10-13.1\n    entries: []\n",
		"an empty chain":             base + "chains:\n  eip155:8453:\n",
		"a chain with a chain":       base + chain + "      []\n    chains: {eip155:1: {version: 2026-10-13.1}}\n",
		"a chain version unformed":   base + "chains:\n  eip155:8453:\n    version: soon\n    entries: []\n",
		"a code twice":               base + chain + cdp + strings.Replace(cdp, "id: cdp", "id: cdp2", 1),
		"an empty code":              base + chain + strings.Replace(cdp, "cdp_facil1", `""`, 1),
		"a code with a comma":        base + chain + strings.Replace(cdp, "cdp_facil1", `"a,b"`, 1),
		"a code with a bar":          base + chain + strings.Replace(cdp, "cdp_facil1", `"a|b"`, 1),
		"a code with a space":        base + chain + strings.Replace(cdp, "cdp_facil1", `" cdp"`, 1),
		"a code on a test relayer":   base + chain + strings.Replace(cdp, "class: facilitator", "class: test", 1),
	}
	for name, body := range cases {
		if r, err := Load(write(t, body)); err == nil {
			t.Errorf("%s accepted: %+v", name, r)
		}
	}
	if _, err := Load(write(t, base+chain+cdp)); err != nil {
		t.Fatalf("a good chain was rejected: %v", err)
	}
}
