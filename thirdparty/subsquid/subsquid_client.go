// thirdparty/subsquid_client.go
package thirdparty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// SubsquidClient translates a subset of JSON-RPC calls into Subsquid GraphQL queries.
type SubsquidClient struct {
	logger    *zerolog.Logger
	projectId string
	upstream  common.Upstream
	endpoint  *url.URL
	http      *http.Client
	vendor    common.Vendor // optional; only used if you wire it
}

var _ clients.ClientInterface = (*SubsquidClient)(nil)

func NewSubsquidClient(
	_ context.Context,
	logger *zerolog.Logger,
	projectId string,
	upstream common.Upstream,
	endpoint *url.URL,
) (*SubsquidClient, error) {
	hc := &http.Client{Timeout: 25 * time.Second}
	lg := logger.With().Str("client", "subsquid").Logger()
	return &SubsquidClient{
		logger:    &lg,
		projectId: projectId,
		upstream:  upstream,
		endpoint:  endpoint,
		http:      hc,
	}, nil
}

func (c *SubsquidClient) GetType() clients.ClientType {
	// Keep it in the HTTP bucket so upstream.Forward switch continues to work.
	return clients.ClientTypeHttpJsonRpc
}

func (c *SubsquidClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}

	method, err := req.Method()
	if err != nil {
		return nil, err
	}

	switch method {
	case "eth_getLogs":
		return c.handleEthGetLogs(ctx, req)

	case "eth_getTransactionByHash":
		return c.handleEthGetTransactionByHash(ctx, req)

	case "eth_getTransactionReceipt":
		return c.handleEthGetTransactionReceipt(ctx, req)

	case "eth_getBlockByHash", "eth_getBlockByNumber":
		return c.handleEthGetBlock(ctx, req)

	default:
		// Tell router this upstream doesn't handle the method -> fallback
		return nil, common.NewErrEndpointUnsupported(fmt.Errorf("method %q not supported by upstream %q", method, c.upstream.Id()))
	}
}

// ---------- Phase 2: eth_getLogs ----------

type getLogsParams struct {
	FromBlock interface{}   `json:"fromBlock,omitempty"`
	ToBlock   interface{}   `json:"toBlock,omitempty"`
	Address   interface{}   `json:"address,omitempty"`
	Topics    []interface{} `json:"topics,omitempty"`
	BlockHash string        `json:"blockHash,omitempty"`
}

func (c *SubsquidClient) handleEthGetLogs(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	// Parse JSON-RPC params via the inner JsonRpcRequest
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	raw0, err := jrq.PeekByPath(0)
	if err != nil && !strings.Contains(err.Error(), "empty params") {
		return nil, fmt.Errorf("eth_getLogs params: %w", err)
	}

	var p getLogsParams
	if raw0 != nil {
		b, _ := json.Marshal(raw0)
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("eth_getLogs params decode: %w", err)
		}
	}

	// Normalize filters
	var fromNum *uint64
	var toNum *uint64
	if p.BlockHash == "" {
		if v, ok := normalizeBlockRef(p.FromBlock); ok {
			fromNum = &v
		}
		if v, ok := normalizeBlockRef(p.ToBlock); ok {
			toNum = &v
		}
	}
	addrs := normalizeAddresses(p.Address)
	topics := normalizeTopics(p.Topics)

	// GraphQL query (adjust "logs" shape to the concrete Subsquid schema if needed)
	query := `
query($where: LogsBoolExp, $limit: Int, $after: String) {
  logs: logs(where: $where, limit: $limit, after: $after, orderBy: [BLOCK_NUMBER_ASC, INDEX_ASC]) {
    edges {
      node {
        address
        data
        topics
        blockNumber
        blockHash
        index
        transactionIndex
        transactionHash
      }
      cursor
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}
`
	where := map[string]any{}
	if p.BlockHash != "" {
		where["blockHash_eq"] = toLowerHex(p.BlockHash)
	} else {
		if fromNum != nil {
			where["blockNumber_gte"] = int64(*fromNum)
		}
		if toNum != nil {
			where["blockNumber_lte"] = int64(*toNum)
		}
	}
	if len(addrs) == 1 {
		where["address_eq"] = strings.ToLower(addrs[0])
	} else if len(addrs) > 1 {
		where["address_in"] = addrsLower(addrs)
	}
	// Topic slots 0..3
	for i, spec := range topics {
		if len(spec) == 0 {
			continue
		}
		if len(spec) == 1 {
			where[fmt.Sprintf("topic%v_eq", i)] = toLowerHex(spec[0])
		} else {
			where[fmt.Sprintf("topic%v_in", i)] = toLowerHexSlice(spec)
		}
	}

	vars := map[string]any{
		"where": where,
		"limit": 5000,
	}

	// Result paging containers
	type edgeNode struct {
		Address          string   `json:"address"`
		Data             string   `json:"data"`
		Topics           []string `json:"topics"`
		BlockNumber      uint64   `json:"blockNumber"`
		BlockHash        string   `json:"blockHash"`
		Index            uint64   `json:"index"`
		TransactionIndex uint64   `json:"transactionIndex"`
		TransactionHash  string   `json:"transactionHash"`
	}
	type logsGQLResult struct {
		Data struct {
			Logs struct {
				Edges []struct {
					Node   edgeNode `json:"node"`
					Cursor string   `json:"cursor"`
				} `json:"edges"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"logs"`
		} `json:"data"`
		Errors any `json:"errors,omitempty"`
	}

	var out []map[string]any
	var after *string
	for {
		if after != nil && *after != "" {
			vars["after"] = *after
		} else {
			delete(vars, "after")
		}

		var res logsGQLResult
		if err := c.postGraphQL(ctx, query, vars, &res); err != nil {
			return nil, err
		}
		// Optional vendor-specific shaping
		if c.vendor != nil && res.Errors != nil {
			if gerr := c.vendor.GetVendorSpecificErrorIfAny(req, nil, res.Errors, nil); gerr != nil {
				return nil, gerr
			}
		}

		for _, e := range res.Data.Logs.Edges {
			n := e.Node
			// Post-filter topics if server-side filters are weaker than needed
			if !matchesTopicSpec(n.Topics, topics) {
				continue
			}
			obj := map[string]any{
				"address":          toLowerHex(n.Address),
				"topics":           toLowerHexSlice(n.Topics),
				"data":             ensure0x(n.Data),
				"blockNumber":      toHexU64(n.BlockNumber),
				"blockHash":        toLowerHex(n.BlockHash),
				"transactionHash":  toLowerHex(n.TransactionHash),
				"transactionIndex": toHexU64(n.TransactionIndex),
				"logIndex":         toHexU64(n.Index),
				"removed":          false,
			}
			out = append(out, obj)
		}

		if !res.Data.Logs.PageInfo.HasNextPage {
			break
		}
		ac := res.Data.Logs.PageInfo.EndCursor
		after = &ac
		if len(out) > 200_000 { // safety
			break
		}
	}

	jrr, err := common.NewJsonRpcResponse(jrq.ID, out, nil)
	if err != nil {
		return nil, err
	}
	nr := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr)
	return nr, nil
}

// ---------- Phase 3: stubs to fill later ----------

func (c *SubsquidClient) handleEthGetTransactionByHash(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	hashI, err := jrq.PeekByPath(0)
	if err != nil || hashI == nil {
		return nil, fmt.Errorf("missing tx hash")
	}
	_ = toLowerHex(hashI.(string))

	// TODO: Build GQL, map to JSON-RPC transaction object
	jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

func (c *SubsquidClient) handleEthGetTransactionReceipt(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	hashI, err := jrq.PeekByPath(0)
	if err != nil || hashI == nil {
		return nil, fmt.Errorf("missing tx hash")
	}
	_ = toLowerHex(hashI.(string))

	// TODO: Build GQL join, map to JSON-RPC receipt (all integers -> 0x hex)
	jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

func (c *SubsquidClient) handleEthGetBlock(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	// TODO: detect hash vs number, fetch block, respect fullTx flag
	jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// ---------- HTTP + helpers ----------

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

func (c *SubsquidClient) postGraphQL(ctx context.Context, query string, vars map[string]any, out interface{}) error {
	body := gqlRequest{Query: query, Variables: vars}
	b, _ := json.Marshal(body)

	u := *c.endpoint // copy
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("build gql request: %w", err),
			map[string]interface{}{
				"error": c.upstream.Id(),
			},
			500,
		)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return common.NewErrEndpointServerSideException(fmt.Errorf("http request failed: %w", err),
			map[string]interface{}{
				"error": c.upstream.Id(),
			},
			500,
		)
	}
	defer resp.Body.Close()

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("read gql response: %w", err), map[string]interface{}{
				"error": c.upstream.Id(),
			},
			500,
		)
	}
	if resp.StatusCode/100 != 2 {
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid http %d: %s", resp.StatusCode, string(rb)),
			map[string]interface{}{
				"error": c.upstream.Id(),
			},
			500,
		)
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorServerSideException,
			"cannot parse json-rpc response: "+err.Error(),
			err,
			nil,
		)
	}
	return nil
}

// ---------- normalization utilities ----------

func normalizeBlockRef(v interface{}) (uint64, bool) {
	switch t := v.(type) {
	case string:
		switch t {
		case "", "latest":
			return 0, false
		case "earliest":
			return 0, true
		default:
			if u, err := common.HexToUint64(t); err == nil {
				return u, true
			}
			return 0, false
		}
	case map[string]interface{}:
		// EIP-1898 object
		if bn, ok := t["blockNumber"]; ok {
			if s, ok := bn.(string); ok {
				if u, err := common.HexToUint64(s); err == nil {
					return u, true
				}
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

func normalizeAddresses(v interface{}) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{ensure0x(strings.ToLower(t))}
	case []interface{}:
		var out []string
		for _, it := range t {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, ensure0x(strings.ToLower(s)))
			}
		}
		return out
	default:
		return nil
	}
}

func normalizeTopics(spec []interface{}) [][]string {
	out := make([][]string, 4)
	for i := 0; i < len(spec) && i < 4; i++ {
		slot := spec[i]
		switch tv := slot.(type) {
		case nil:
			out[i] = nil
		case string:
			if tv == "" {
				out[i] = nil
			} else {
				out[i] = []string{ensure0x(strings.ToLower(tv))}
			}
		case []interface{}:
			var vals []string
			for _, it := range tv {
				if s, ok := it.(string); ok && s != "" {
					vals = append(vals, ensure0x(strings.ToLower(s)))
				}
			}
			if len(vals) == 0 {
				out[i] = nil
			} else {
				out[i] = vals
			}
		default:
			out[i] = nil
		}
	}
	return out
}

func matchesTopicSpec(topics []string, spec [][]string) bool {
	for i := 0; i < len(spec) && i < 4; i++ {
		want := spec[i]
		if len(want) == 0 {
			continue
		}
		if i >= len(topics) {
			return false
		}
		got := strings.ToLower(ensure0x(topics[i]))
		match := false
		for _, w := range want {
			if strings.ToLower(ensure0x(w)) == got {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

func ensure0x(s string) string {
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return "0x" + strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	}
	return "0x" + s
}

func toLowerHex(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(ensure0x(s))
}

func toLowerHexSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, toLowerHex(s))
	}
	return out
}

func addrsLower(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(ensure0x(s)))
	}
	return out
}

func toHexU64(u uint64) string {
	// Ethereum quantity hex (no leading zeros)
	if u == 0 {
		return "0x0"
	}
	return fmt.Sprintf("0x%x", u)
}
