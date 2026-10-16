package server

import (
	"cmp"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"x402/internal/config"
	"x402/internal/testkit"
)

const specHost = "http://127.0.0.1:8181"

type specCase struct {
	name, path string
	// like names the documented path whose operation answers for a path that no operation names
	like   string
	header []string
	status int
	// malformed marks a request that the spec itself rejects
	malformed bool
	gone      bool
	// handler replaces the seeded handler for a case that needs another state of the service
	handler func(t *testing.T) http.Handler
}

var stampedOperations = []string{"listLeaderboard", "listFlows", "listRulesDaily", "getEntityDaily", "listPayments", "getTxPayments"}

func loadSpec(t testing.TB) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func withoutIndexer(t *testing.T) http.Handler {
	d, cfg, reg := load(t)
	return handlerFor(t, d.Store, reg, []config.Config{cfg}, NewMetrics(), func() bool { return false })
}

func baseHandler(t *testing.T) http.Handler { return buildTwoChains(t).h }

func withBrokenView(t *testing.T) http.Handler {
	w := build(t)
	if _, err := w.d.Store.Pool.Exec(t.Context(), `drop view payment_signals`); err != nil {
		t.Fatal(err)
	}
	return w.h
}

func servedCases() []specCase {
	return []specCase{
		{name: "healthz", path: "/healthz"},
		{name: "readyz", path: "/readyz"},
		{name: "readyz while not ready", path: "/readyz", status: http.StatusServiceUnavailable, handler: withoutIndexer},
		{name: "status", path: base + "/status"},
		{name: "registry", path: base + "/registry"},
		{name: "facilitator leaderboard", path: base + "/leaderboards/facilitators"},
		{name: "facilitator leaderboard with both tiers", path: base + "/leaderboards/facilitators?tier=AB&order=raw_volume&limit=100"},
		{name: "seller leaderboard", path: base + "/leaderboards/sellers?order=genuine_payments&limit=5"},
		{name: "payer leaderboard in a past window", path: base + "/leaderboards/payers?from=2026-02-03&to=2026-02-03"},
		{name: "flows", path: base + "/flows"},
		{name: "flows of both tiers by genuine payments", path: base + "/flows?tier=AB&order=genuine_payments&limit=3"},
		{name: "flows by raw volume", path: base + "/flows?order=raw_volume"},
		{name: "base flows", path: baseRoot + "/flows", handler: baseHandler},
		{name: "rules daily", path: base + "/rules/daily"},
		{name: "base rules daily", path: baseRoot + "/rules/daily", handler: baseHandler},
		{name: "facilitator daily", path: base + "/facilitators/monad-foundation/daily"},
		{name: "facilitator daily of the unlisted relayer", path: base + "/facilitators/" + unlisted + "/daily?tier=AB"},
		{name: "seller daily", path: base + "/sellers/" + burstSeller + "/daily"},
		{name: "payer daily in uppercase", path: base + "/payers/" + upperHex(pagingPayer) + "/daily"},
		{name: "daily of a known seller outside the window", path: base + "/sellers/" + burstSeller + "/daily?from=2020-01-01&to=2020-01-31"},
		{name: "payments first page", path: base + "/payments?limit=2"},
		{name: "payments last page", path: base + "/payments?payer=" + pagingPayer + "&limit=120"},
		{name: "payment with a 256-bit amount", path: base + "/payments?payer=" + upperCasePayer + "&tier=all"},
		{name: "payments of a tier X relayer", path: base + "/payments?relayer=" + testRelayer + "&tier=all"},
		{name: "payments by method", path: base + "/payments?method=eip3009_transfer&facilitator=monad-foundation&limit=1"},
		{name: "payments after a cursor", path: base + "/payments?payer=" + pagingPayer + "&limit=1&cursor=5039.2"},
		{name: "tx", path: base + "/payments/" + twoTx},
		{name: "tx by uppercase hash", path: base + "/payments/" + upperHex(twoTx)},
		{name: "tx of a tier X row", path: base + "/payments/" + settleTx},

		{name: "base status", path: baseRoot + "/status", handler: baseHandler},
		{name: "base registry", path: baseRoot + "/registry", handler: baseHandler},
		{name: "base facilitator leaderboard", path: baseRoot + "/leaderboards/facilitators?tier=AB", handler: baseHandler},
		{name: "base facilitator daily", path: baseRoot + "/facilitators/coinbase-cdp/daily", handler: baseHandler},
		{name: "base payments", path: baseRoot + "/payments", handler: baseHandler},
		{name: "base tx", path: baseRoot + "/payments/" + testkit.Hash(200), handler: baseHandler},
		{name: "base routed payments of every tier", path: baseRoot + "/payments?method=eip3009_routed&tier=all", handler: routedHandler},

		{name: "limit zero", path: base + "/payments?limit=0", status: http.StatusBadRequest, malformed: true},
		{name: "leaderboard limit 101", path: base + "/leaderboards/sellers?limit=101", status: http.StatusBadRequest, malformed: true},
		{name: "from after to", path: base + "/leaderboards/facilitators?from=2026-02-02&to=2026-02-01", status: http.StatusBadRequest},
		{name: "unknown parameter", path: base + "/payments?foo=1", status: http.StatusBadRequest},
		{name: "flows with a bad date", path: base + "/flows?from=2026-13-01", status: http.StatusBadRequest, malformed: true},
		{name: "flows with an unknown parameter", path: base + "/flows?foo=1", status: http.StatusBadRequest},
		{name: "rules daily with an unknown parameter", path: base + "/rules/daily?tier=A", status: http.StatusBadRequest},
		{name: "span of 367 days", path: base + "/facilitators/monad-foundation/daily?from=2025-01-01&to=2026-01-02", status: http.StatusBadRequest},
		{name: "short tx hash", path: base + "/payments/0x12", status: http.StatusBadRequest, malformed: true},
		{name: "short payer", path: base + "/payments?payer=0x12", status: http.StatusBadRequest, malformed: true},
		{name: "chain that is not CAIP-2", path: "/internal/v1/chains/foo/x402/status", status: http.StatusBadRequest, malformed: true},
		{name: "leaderboard kind that does not exist", path: base + "/leaderboards/bogus", status: http.StatusBadRequest, malformed: true},
		{name: "facilitator that is nothing", path: base + "/facilitators/nope/daily", status: http.StatusBadRequest},

		{name: "another chain", path: "/internal/v1/chains/eip155:1/x402/status", status: http.StatusNotFound},
		{name: "unknown tx", path: base + "/payments/" + unknownTx, status: http.StatusNotFound},
		{name: "unknown seller", path: base + "/sellers/" + testkit.Addr(9999) + "/daily", status: http.StatusNotFound},
		{name: "unknown route", path: "/nope", like: base + "/status", status: http.StatusNotFound},

		{name: "payments before the indexer exists", path: base + "/payments", status: http.StatusServiceUnavailable, handler: withoutIndexer},
		{name: "status needs only the database", path: base + "/status", handler: withoutIndexer},
		{name: "status past its deadline", path: base + "/status", header: []string{"Sona-Deadline-Ms", "0"}, status: http.StatusGatewayTimeout},
		{name: "payments past their deadline", path: base + "/payments", header: []string{"Sona-Deadline-Ms", "0"}, status: http.StatusGatewayTimeout},
		{name: "registry past its deadline", path: base + "/registry", header: []string{"Sona-Deadline-Ms", "0"}, status: http.StatusGatewayTimeout},
		{name: "payments of a client that left", path: base + "/payments", status: statusClientClosed, gone: true},
		{name: "payments the database rejects", path: base + "/payments", status: http.StatusInternalServerError, handler: withBrokenView},
	}
}

func checkHeaders(t *testing.T, operation string, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Header()
	probe := operation == "getHealth" || operation == "getReady"
	if got := h.Get("X-Request-Id") != ""; got == probe {
		t.Errorf("X-Request-Id present %v on %s", got, operation)
	}
	if rec.Code == http.StatusServiceUnavailable && !probe && h.Get("Retry-After") != "1" {
		t.Errorf("Retry-After %q", h.Get("Retry-After"))
	}
	if rec.Code == statusClientClosed && rec.Body.Len() != 0 {
		t.Errorf("body %q", rec.Body)
	}
	stamped := rec.Code == http.StatusOK && slices.Contains(stampedOperations, operation)
	for _, name := range []string{"Sona-Finality", "Sona-Block", "Sona-Block-Time"} {
		if got := h.Get(name) != ""; got != stamped {
			t.Errorf("%s present %v, want %v", name, got, stamped)
		}
	}
}

func checkDeclared(t *testing.T, op *openapi3.Operation, rec *httptest.ResponseRecorder) {
	t.Helper()
	ref := cmp.Or(op.Responses.Status(rec.Code), op.Responses.Default())
	declared := make(map[string]bool)
	for name := range ref.Value.Headers {
		declared[http.CanonicalHeaderKey(name)] = true
	}
	for name := range rec.Header() {
		if !declared[name] && name != "Content-Type" && name != "Content-Length" {
			t.Errorf("%d sends %s, which the spec does not declare", rec.Code, name)
		}
	}
}

func TestSpecIsValid(t *testing.T) {
	doc := loadSpec(t)
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.2" {
		t.Errorf("openapi %q", doc.OpenAPI)
	}
	var urls []string
	for _, s := range doc.Servers {
		urls = append(urls, s.URL)
	}
	if want := []string{specHost}; !slices.Equal(urls, want) {
		t.Fatalf("servers %q, want %q", urls, want)
	}
}

func TestSpecIsSelfContained(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`\$ref"?:\s*["']?([^"'\s,}]+)`).FindAllStringSubmatch(string(raw), -1)
	if len(refs) == 0 {
		t.Fatal("the file holds no $ref, so the pattern or the spec is wrong")
	}
	for _, m := range refs {
		if !strings.HasPrefix(m[1], "#/") {
			t.Errorf("$ref %s leaves the file", m[1])
		}
	}
}

func TestSpecOperationsAreTheRoutes(t *testing.T) {
	doc := loadSpec(t)
	var documented, served []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented = append(documented, method+" "+path)
		}
	}
	for _, rt := range routes {
		served = append(served, rt.pattern)
	}
	slices.Sort(documented)
	slices.Sort(served)
	if !slices.Equal(documented, served) {
		t.Fatalf("spec has %q, routes are %q", documented, served)
	}
}

func TestSpecValidatesResponses(t *testing.T) {
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	seededHandler := build(t).h
	for _, c := range servedCases() {
		t.Run(c.name, func(t *testing.T) {
			h := seededHandler
			if c.handler != nil {
				h = c.handler(t)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.gone {
				cancel()
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, specHost+c.path, nil)
			for i := 0; i+1 < len(c.header); i += 2 {
				req.Header.Set(c.header[i], c.header[i+1])
			}
			lookup := req
			if c.like != "" {
				lookup = httptest.NewRequestWithContext(t.Context(), http.MethodGet, specHost+c.like, nil)
			}
			route, params, err := router.FindRoute(lookup)
			if err != nil {
				t.Fatal(err)
			}
			in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}
			if c.like == "" {
				if err := openapi3filter.ValidateRequest(t.Context(), in); (err != nil) != c.malformed {
					t.Fatalf("malformed %v, request: %v", c.malformed, err)
				}
			}
			want := cmp.Or(c.status, http.StatusOK)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body)
			}
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in,
				Status:                 rec.Code,
				Header:                 rec.Header(),
				Options:                &openapi3filter.Options{IncludeResponseStatus: true},
			}
			if err := openapi3filter.ValidateResponse(t.Context(), out.SetBodyBytes(rec.Body.Bytes())); err != nil {
				t.Fatalf("response: %v\n%s", err, rec.Body)
			}
			checkHeaders(t, route.Operation.OperationID, rec)
			checkDeclared(t, route.Operation, rec)
		})
	}
}

func TestEveryOperationDeclaresItsProblems(t *testing.T) {
	data := []string{"400", "404", "500", "503", "504"}
	want := map[string][]string{
		"getHealth":       {"200"},
		"getReady":        {"200", "503"},
		"getX402Status":   data,
		"getX402Registry": {"400", "404", "504"},
		"listLeaderboard": data,
		"listFlows":       data,
		"listRulesDaily":  data,
		"getEntityDaily":  data,
		"listPayments":    data,
		"getTxPayments":   data,
	}
	got := make(map[string]*openapi3.Operation)
	for _, item := range loadSpec(t).Paths.Map() {
		for _, op := range item.Operations() {
			got[op.OperationID] = op
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d operations, want %d", len(got), len(want))
	}
	for id, codes := range want {
		op := got[id]
		if op == nil {
			t.Errorf("operation %s is not documented", id)
			continue
		}
		for _, code := range codes {
			if op.Responses.Value(code) == nil {
				t.Errorf("%s does not declare %s", id, code)
			}
		}
	}
}
