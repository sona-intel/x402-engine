package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Chain           string
	ChainID         int
	RefreshInterval time.Duration
	MarginBlocks    int64
	SourceSchema    string
	IngestBatchRows int
	Rules           Rules
	BuilderCodes    bool
	RetentionDays   int
	HistoryFrom     string
}

type Rules struct {
	Version            string  `yaml:"version" json:"version"`
	VolumeToken        string  `yaml:"-" json:"volume_token"`
	FixedAmount        Share   `yaml:"fixed_amount" json:"fixed_amount"`
	PayeeConcentration Share   `yaml:"payee_concentration" json:"payee_concentration"`
	NoOtherActivity    MinRows `yaml:"no_other_activity" json:"no_other_activity"`
	BridgeFunded       Cluster `yaml:"bridge_funded" json:"bridge_funded"`
}

type Share struct {
	MinRows int     `yaml:"min_rows" json:"min_rows"`
	Share   float64 `yaml:"share" json:"share"`
}

type MinRows struct {
	MinRows int `yaml:"min_rows" json:"min_rows"`
}

type Cluster struct {
	MinRecipients int `yaml:"min_recipients" json:"min_recipients"`
}

type chainFile struct {
	ID           string `yaml:"id"`
	SourceSchema string `yaml:"source_schema"`
	VolumeToken  string `yaml:"volume_token"`
	BuilderCodes bool   `yaml:"builder_codes"`
	Retention    int    `yaml:"retention_days"`
	HistoryFrom  string `yaml:"history_from"`
}

type file struct {
	Chains          []chainFile   `yaml:"chains"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
	MarginBlocks    int64         `yaml:"margin_blocks"`
	IngestBatchRows int           `yaml:"ingest_batch_rows"`
	Rules           Rules         `yaml:"rules"`
}

var (
	schemaName   = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	tokenAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	chainFormat  = regexp.MustCompile(`^eip155:([1-9][0-9]{0,9})$`)
)

func Load(dir string) ([]Config, error) {
	path := filepath.Join(dir, "engine.yaml")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var raw file
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("engine config %s: %w", path, err)
	}
	cfgs, err := raw.configs()
	if err != nil {
		return nil, fmt.Errorf("engine config %s: %w", path, err)
	}
	return cfgs, nil
}

func (f file) configs() ([]Config, error) {
	switch {
	case len(f.Chains) == 0:
		return nil, errors.New("chains must list at least one chain")
	case f.RefreshInterval <= 0:
		return nil, fmt.Errorf("refresh_interval must be positive, got %s", f.RefreshInterval)
	case f.MarginBlocks < 0:
		return nil, fmt.Errorf("margin_blocks must not be negative, got %d", f.MarginBlocks)
	case f.IngestBatchRows <= 0:
		return nil, fmt.Errorf("ingest_batch_rows must be positive, got %d", f.IngestBatchRows)
	}
	if err := f.Rules.validate(); err != nil {
		return nil, err
	}
	out := make([]Config, len(f.Chains))
	schemas := map[string]bool{}
	for i, c := range f.Chains {
		m := chainFormat.FindStringSubmatch(c.ID)
		switch {
		case m == nil:
			return nil, fmt.Errorf("chain %q is not eip155 and a number", c.ID)
		case !schemaName.MatchString(c.SourceSchema):
			return nil, fmt.Errorf("chain %s: source_schema %q is not a lowercase identifier", c.ID, c.SourceSchema)
		case !tokenAddress.MatchString(c.VolumeToken):
			return nil, fmt.Errorf("chain %s: volume_token %q is not a 0x address", c.ID, c.VolumeToken)
		case c.Retention < 0:
			return nil, fmt.Errorf("chain %s: retention_days must not be negative, got %d", c.ID, c.Retention)
		case schemas[c.SourceSchema]:
			return nil, fmt.Errorf("chain %s: source_schema %q is used by another chain", c.ID, c.SourceSchema)
		}
		if c.HistoryFrom != "" {
			if _, err := time.Parse(time.DateOnly, c.HistoryFrom); err != nil {
				return nil, fmt.Errorf("chain %s: history_from %q is not YYYY-MM-DD", c.ID, c.HistoryFrom)
			}
		}
		for _, o := range out[:i] {
			if o.Chain == c.ID {
				return nil, fmt.Errorf("chain %s is listed twice", c.ID)
			}
		}
		schemas[c.SourceSchema] = true
		id, _ := strconv.Atoi(m[1])
		rules := f.Rules
		rules.VolumeToken = strings.ToLower(c.VolumeToken)
		out[i] = Config{
			Chain: c.ID, ChainID: id, RefreshInterval: f.RefreshInterval, MarginBlocks: f.MarginBlocks, SourceSchema: c.SourceSchema,
			IngestBatchRows: f.IngestBatchRows, Rules: rules, BuilderCodes: c.BuilderCodes, RetentionDays: c.Retention, HistoryFrom: c.HistoryFrom,
		}
	}
	return out, nil
}

func (r Rules) validate() error {
	switch {
	case r.Version == "":
		return errors.New("rules.version must be set")
	case !r.FixedAmount.valid():
		return fmt.Errorf("rules.fixed_amount needs min_rows above 0 and share in (0.5, 1], got %+v", r.FixedAmount)
	case !r.PayeeConcentration.valid():
		return fmt.Errorf("rules.payee_concentration needs min_rows above 0 and share in (0.5, 1], got %+v", r.PayeeConcentration)
	case r.NoOtherActivity.MinRows <= 0:
		return fmt.Errorf("rules.no_other_activity.min_rows must be positive, got %d", r.NoOtherActivity.MinRows)
	case r.BridgeFunded.MinRecipients <= 0:
		return fmt.Errorf("rules.bridge_funded.min_recipients must be positive, got %d", r.BridgeFunded.MinRecipients)
	}
	return nil
}

func (s Share) valid() bool {
	return s.MinRows > 0 && s.Share > 0.5 && s.Share <= 1
}

func (r Rules) Checksum() string {
	// AllowInvalidUTF8 leaves no encode error to handle
	b, _ := json.Marshal(r, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
