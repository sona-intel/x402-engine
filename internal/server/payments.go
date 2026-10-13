package server

import (
	"encoding/hex"
	"encoding/json/jsontext"
	"net/http"
	"strconv"
	"time"

	"x402/internal/store"
)

type hexBytes []byte

func (h hexBytes) MarshalJSONTo(enc *jsontext.Encoder) error {
	if h == nil {
		return enc.WriteToken(jsontext.Null)
	}
	b := append(make([]byte, 0, 4+2*len(h)), '"', '0', 'x')
	return enc.WriteValue(append(hex.AppendEncode(b, h), '"'))
}

type paymentBody struct {
	ID          string   `json:"id"`
	Chain       string   `json:"chain"`
	Block       int64    `json:"block"`
	BlockTime   string   `json:"block_time"`
	Tx          hexBytes `json:"tx"`
	LogIndex    int32    `json:"log_index"`
	Method      string   `json:"method"`
	Token       hexBytes `json:"token"`
	Payer       hexBytes `json:"payer"`
	Payee       hexBytes `json:"payee"`
	Amount      *string  `json:"amount"`
	Nonce       hexBytes `json:"nonce"`
	ValidAfter  *string  `json:"valid_after"`
	ValidBefore *string  `json:"valid_before"`
	Relayer     hexBytes `json:"relayer"`
	TxTo        hexBytes `json:"tx_to"`
	Selector    hexBytes `json:"selector"`
	GasCost     *string  `json:"gas_cost"`
	Tier        string   `json:"tier"`
	Facilitator *string  `json:"facilitator"`
	Reason      *string  `json:"reason"`
	Genuine     *bool    `json:"genuine"`
	RulesFired  []string `json:"rules_fired"`
}

func paymentBodies(ch *chainApp, rows []store.PaymentRow) []paymentBody {
	out := make([]paymentBody, len(rows))
	for i, r := range rows {
		out[i] = paymentBody{
			ID: ch.prefix + strconv.FormatInt(r.Block, 10) + "_" + strconv.Itoa(int(r.LogIndex)), Chain: ch.name, Block: r.Block,
			BlockTime: time.Unix(r.BlockTime, 0).UTC().Format(time.RFC3339), Tx: r.Tx, LogIndex: r.LogIndex, Method: store.MethodName(r.Method),
			Token: r.Token, Payer: r.Payer, Payee: r.Payee, Amount: r.Amount, Nonce: r.Nonce, ValidAfter: r.ValidAfter, ValidBefore: r.ValidBefore,
			Relayer: r.Relayer, TxTo: r.TxTo, Selector: r.Selector, GasCost: r.GasCost, Tier: r.Tier, Facilitator: r.Facilitator, Reason: r.Reason,
			Genuine: r.Genuine, RulesFired: r.RulesFired,
		}
	}
	return out
}

type paymentsBody struct {
	Kind       string        `json:"kind"`
	Chain      string        `json:"chain"`
	Items      []paymentBody `json:"items"`
	NextCursor string        `json:"next_cursor,omitzero"`
}

func (a *app) payments() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		q, err := parseQuery(c.req, "payer", "payee", "relayer", "facilitator", "method", "tier", "from", "to", "limit", "cursor")
		if err != nil {
			return nil, err
		}
		f, err := c.ch.parseFilter(q)
		if err != nil {
			return nil, err
		}
		m, err := c.data()
		if err != nil {
			return nil, err
		}
		f.UpTo = m.WatermarkBlock
		rows, err := a.st.Payments(c.ctx, c.ch.id, f)
		if err != nil {
			return nil, err
		}
		b := paymentsBody{Kind: "x402_payments", Chain: c.ch.name}
		if len(rows) > f.Limit {
			rows = rows[:f.Limit]
			last := rows[f.Limit-1]
			b.NextCursor = strconv.FormatInt(last.Block, 10) + "." + strconv.Itoa(int(last.LogIndex))
		}
		b.Items = paymentBodies(c.ch, rows)
		return marshal(b), nil
	})
}

type txBody struct {
	Kind  string        `json:"kind"`
	Chain string        `json:"chain"`
	Tx    string        `json:"tx"`
	Items []paymentBody `json:"items"`
}

func (a *app) tx() http.HandlerFunc {
	return a.serve(func(c *call) ([]byte, error) {
		hash, err := parseHex(c.req.PathValue("tx"), 32, "A transaction hash")
		if err != nil {
			return nil, err
		}
		if _, err := parseQuery(c.req); err != nil {
			return nil, err
		}
		m, err := c.data()
		if err != nil {
			return nil, err
		}
		rows, err := a.st.PaymentsByTx(c.ctx, c.ch.id, hash, m.WatermarkBlock)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, missing("No payment was recorded for this transaction.")
		}
		return marshal(txBody{Kind: "x402_tx", Chain: c.ch.name, Tx: lowerHex(hash), Items: paymentBodies(c.ch, rows)}), nil
	})
}
