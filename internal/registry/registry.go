package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Address struct {
	Hex       string `yaml:"address" json:"address"`
	FromBlock *int64 `yaml:"from_block" json:"from_block,omitzero"`
	ToBlock   *int64 `yaml:"to_block" json:"to_block,omitzero"`
}

type Entry struct {
	ID        string    `yaml:"id" json:"id"`
	Name      string    `yaml:"name" json:"name"`
	URL       string    `yaml:"url" json:"url,omitzero"`
	Class     string    `yaml:"class" json:"class"`
	Addresses []Address `yaml:"addresses" json:"addresses"`
	Codes     []string  `yaml:"builder_codes" json:"builder_codes,omitzero"`
}

type Registry struct {
	Version  string  `yaml:"version" json:"version"`
	Checksum string  `yaml:"-" json:"checksum,omitzero"`
	Entries  []Entry `yaml:"entries" json:"entries"`
	// Chains holds the registries of the other chains, so the top level keeps its checksum when one is added
	Chains map[string]*Registry `yaml:"chains" json:"-"`
}

const RootChain = "eip155:143"

var chainFormat = regexp.MustCompile(`^eip155:[1-9][0-9]{0,9}$`)

func (r *Registry) For(chain string) (*Registry, bool) {
	if chain == RootChain {
		return r, true
	}
	c, ok := r.Chains[chain]
	return c, ok
}

func (r *Registry) Codes() (codes, ids []string) {
	for _, e := range r.Entries {
		for _, c := range e.Codes {
			codes, ids = append(codes, c), append(ids, e.ID)
		}
	}
	return codes, ids
}

var (
	versionFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.\d+$`)
	addressFormat = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

func Load(path string) (*Registry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &Registry{}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(r); err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	if err := r.normalise(); err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	r.seal()
	for chain, c := range r.Chains {
		switch {
		case !chainFormat.MatchString(chain) || chain == RootChain:
			return nil, fmt.Errorf("registry %s: chain %q is not another eip155 chain", path, chain)
		case c == nil || len(c.Chains) > 0:
			return nil, fmt.Errorf("registry %s: chain %s needs a version and entries only", path, chain)
		}
		if err := c.normalise(); err != nil {
			return nil, fmt.Errorf("registry %s: chain %s: %w", path, chain, err)
		}
		c.seal()
	}
	return r, nil
}

func (r *Registry) seal() {
	// AllowInvalidUTF8 leaves no encode error to handle
	b, _ := json.Marshal(r, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	sum := sha256.Sum256(b)
	r.Checksum = hex.EncodeToString(sum[:])
}

func (r *Registry) normalise() error {
	if !versionFormat.MatchString(r.Version) {
		return fmt.Errorf("version %q is not YYYY-MM-DD.n", r.Version)
	}
	type key struct {
		address string
		from    int64
	}
	seen := map[key]bool{}
	codes := map[string]bool{}
	for i := range r.Entries {
		e := &r.Entries[i]
		switch e.Class {
		case "facilitator", "unlisted", "test":
		default:
			return fmt.Errorf("entry %q: class %q is not facilitator, unlisted or test", e.ID, e.Class)
		}
		for _, c := range e.Codes {
			if c == "" || strings.TrimSpace(c) != c || strings.ContainsAny(c, "|,") {
				return fmt.Errorf("entry %q: builder code %q is empty or has spaces, commas or bars", e.ID, c)
			}
			if e.Class != "facilitator" {
				return fmt.Errorf("entry %q: only a facilitator takes a builder code", e.ID)
			}
			if codes[c] {
				return fmt.Errorf("entry %q: builder code %q is listed twice", e.ID, c)
			}
			codes[c] = true
		}
		for j := range e.Addresses {
			a := &e.Addresses[j]
			if !addressFormat.MatchString(a.Hex) {
				return fmt.Errorf("entry %q: address %q is not 0x and 40 hex digits", e.ID, a.Hex)
			}
			a.Hex = strings.ToLower(a.Hex)
			var from int64
			if a.FromBlock != nil {
				from = *a.FromBlock
			}
			if a.ToBlock != nil && *a.ToBlock < from {
				return fmt.Errorf("entry %q: address %s ends at block %d before it starts at %d", e.ID, a.Hex, *a.ToBlock, from)
			}
			k := key{a.Hex, from}
			if seen[k] {
				return fmt.Errorf("entry %q: address %s is listed twice from block %d", e.ID, a.Hex, from)
			}
			seen[k] = true
		}
	}
	return nil
}
