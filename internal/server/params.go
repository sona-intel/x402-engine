package server

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"x402/internal/problem"
	"x402/internal/store"
)

const (
	maxSpanDays     = 366
	maxFlowDays     = 90
	defaultSpanDays = 30
)

var (
	tierA  = []string{"A"}
	tierAB = []string{"A", "B"}
)

type httpError struct{ code, detail string }

func (e *httpError) Error() string { return e.code + ": " + e.detail }

func invalidQuery(detail string) error { return &httpError{problem.CodeInvalidQuery, detail} }

func invalidID(detail string) error { return &httpError{problem.CodeInvalidID, detail} }

func missing(detail string) error { return &httpError{problem.CodeNotFound, detail} }

func (a *app) chain(s string) (*chainApp, error) {
	ns, ref, found := strings.Cut(s, ":")
	if !found || !shaped(ns, 3, 8, false) || !shaped(ref, 1, 32, true) {
		return nil, invalidID("The chain is not a CAIP-2 identifier.")
	}
	ch, ok := a.chains[s]
	if !ok {
		return nil, &httpError{problem.CodeUnsupported, "Only " + strings.Join(a.names(), ", ") + " " + plural(len(a.chains), "is", "are") + " served."}
	}
	return ch, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func shaped(s string, lo, hi int, reference bool) bool {
	if len(s) < lo || len(s) > hi {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		case reference && (c >= 'A' && c <= 'Z' || c == '_'):
		default:
			return false
		}
	}
	return true
}

func parseQuery(req *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return nil, invalidQuery("The query string is malformed.")
	}
	for k, v := range q {
		if !slices.Contains(allowed, k) {
			return nil, invalidQuery("Unknown parameter " + strconv.Quote(k[:min(len(k), 40)]) + ".")
		}
		if len(v) != 1 || v[0] == "" {
			return nil, invalidQuery("Parameter " + k + " takes exactly one non-empty value.")
		}
	}
	return q, nil
}

func parseDay(s string) (int, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return 0, invalidQuery("Days are written YYYY-MM-DD.")
	}
	return int(t.Unix() / 86400), nil
}

func formatDay(day int) string {
	return time.Unix(int64(day)*86400, 0).UTC().Format(time.DateOnly)
}

type window struct{ from, to *int }

func optionalDay(q url.Values, name string) (*int, error) {
	v := q.Get(name)
	if v == "" {
		return nil, nil
	}
	day, err := parseDay(v)
	return &day, err
}

func parseWindow(q url.Values) (window, error) {
	from, err := optionalDay(q, "from")
	if err != nil {
		return window{}, err
	}
	to, err := optionalDay(q, "to")
	if err != nil {
		return window{}, err
	}
	if from != nil && to != nil && *from > *to {
		return window{}, invalidQuery("from is after to.")
	}
	return window{from, to}, nil
}

func (w window) resolve(watermarkDay, maxDays int) (from, to int, err error) {
	to = watermarkDay
	if w.to != nil {
		to = *w.to
	}
	from = to - defaultSpanDays + 1
	if w.from != nil {
		from = *w.from
	}
	switch {
	case from > to:
		return 0, 0, invalidQuery("from is after to.")
	case to-from+1 > maxDays:
		return 0, 0, invalidQuery("The window is longer than " + strconv.Itoa(maxDays) + " days.")
	}
	return from, to, nil
}

func parseTier(q url.Values, allowAll bool) (string, []string, error) {
	switch v := q.Get("tier"); {
	case v == "" || v == "A":
		return "A", tierA, nil
	case v == "AB":
		return v, tierAB, nil
	case v == "all" && allowAll:
		return v, nil, nil
	}
	if allowAll {
		return "", nil, invalidQuery("tier is A, AB or all.")
	}
	return "", nil, invalidQuery("tier is A or AB.")
}

func parseLimit(q url.Values, def, most int) (int, error) {
	v := q.Get("limit")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 31)
	if err != nil || n < 1 || int(n) > most {
		return 0, invalidQuery("limit is a whole number from 1 to " + strconv.Itoa(most) + ".")
	}
	return int(n), nil
}

func parseOrder(q url.Values) (string, error) {
	switch v := q.Get("order"); v {
	case "":
		return "genuine_volume", nil
	case "genuine_volume", "raw_volume", "genuine_payments", "raw_payments":
		return v, nil
	}
	return "", invalidQuery("order is genuine_volume, raw_volume, genuine_payments or raw_payments.")
}

func parseCursor(s string) (*store.Cursor, error) {
	b, l, found := strings.Cut(s, ".")
	block, errBlock := strconv.ParseUint(b, 10, 63)
	logIndex, errLog := strconv.ParseUint(l, 10, 31)
	if !found || errBlock != nil || errLog != nil {
		return nil, invalidQuery("cursor is the next_cursor of a previous page.")
	}
	return &store.Cursor{Block: int64(block), LogIndex: int32(logIndex)}, nil
}

func parseHex(s string, size int, what string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if !strings.HasPrefix(s, "0x") || err != nil || len(b) != size {
		return nil, invalidID(what + " is 0x and " + strconv.Itoa(2*size) + " hex digits.")
	}
	return b, nil
}

func parseAddress(s string) ([]byte, error) { return parseHex(s, 20, "An address") }

func lowerHex(b []byte) string { return "0x" + hex.EncodeToString(b) }

func parseEntity(s string) (string, error) {
	b, err := parseAddress(s)
	if err != nil {
		return "", err
	}
	return lowerHex(b), nil
}

func (ch *chainApp) parseFacilitator(s string) (string, error) {
	if s == "unknown" || ch.ids[s] {
		return s, nil
	}
	if b, err := parseAddress(s); err == nil {
		return lowerHex(b), nil
	}
	return "", invalidID("A facilitator is a registry id, a relayer address or unknown.")
}

func (ch *chainApp) parseFilter(q url.Values) (store.PaymentFilter, error) {
	var f store.PaymentFilter
	var err error
	for _, p := range []struct {
		name string
		dst  *[]byte
	}{{"payer", &f.Payer}, {"payee", &f.Payee}, {"relayer", &f.Relayer}} {
		if v := q.Get(p.name); v != "" {
			if *p.dst, err = parseAddress(v); err != nil {
				return f, err
			}
		}
	}
	if v := q.Get("facilitator"); v != "" {
		if f.Facilitator, err = ch.parseFacilitator(v); err != nil {
			return f, err
		}
	}
	if v := q.Get("method"); v != "" {
		var ok bool
		if f.Method, ok = store.MethodOrdinal(v); !ok {
			return f, invalidQuery("method is one of the methods the payment objects carry.")
		}
	}
	if _, f.Tiers, err = parseTier(q, true); err != nil {
		return f, err
	}
	win, err := parseWindow(q)
	if err != nil {
		return f, err
	}
	f.FromDay, f.ToDay = win.from, win.to
	if f.Limit, err = parseLimit(q, 50, 200); err != nil {
		return f, err
	}
	if v := q.Get("cursor"); v != "" {
		f.After, err = parseCursor(v)
	}
	return f, err
}
