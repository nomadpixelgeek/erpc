// thirdparty/subsquid_client.go
package thirdparty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
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
		// return nil, common.NewErrEndpointUnsupported(fmt.Errorf("method %q not supported by upstream %q", method, c.upstream.Id()))
		// Using the standard “skipped” error ensures Forward() treats it correctly.
		return nil, common.NewErrUpstreamRequestSkipped(
			common.NewErrUpstreamMethodIgnored(method, c.upstream.Id()),
			c.upstream.Id(),
		)
	}
}

// ---------- eth_getLogs ----------

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

// ---------- eth_getTransactionByHash ----------

func (c *SubsquidClient) handleEthGetTransactionByHash(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	hashI, err := jrq.PeekByPath(0)
	if err != nil || hashI == nil {
		return nil, fmt.Errorf("missing tx hash")
	}
	txHash := toLowerHex(hashI.(string))

	// GraphQL: pull a single transaction by hash
	query := `
query($hash: String!) {
  transactions: transactions(where: { hash_eq: $hash }, limit: 1) {
    edges {
      node {
        hash
        from
        to
        value
        input
        nonce
        gas
        gasPrice
        blockNumber
        blockHash
        transactionIndex
        r
        s
        v
      }
    }
  }
}
`

	type txNode struct {
		Hash             string  `json:"hash"`
		From             string  `json:"from"`
		To               *string `json:"to"`
		Value            string  `json:"value"` // often decimal as string
		Input            string  `json:"input"`
		Nonce            uint64  `json:"nonce"`
		Gas              uint64  `json:"gas"`
		GasPrice         string  `json:"gasPrice"` // decimal as string
		BlockNumber      uint64  `json:"blockNumber"`
		BlockHash        string  `json:"blockHash"`
		TransactionIndex uint64  `json:"transactionIndex"`
		R                string  `json:"r"`
		S                string  `json:"s"`
		V                uint64  `json:"v"`
	}
	var gqlResp struct {
		Data struct {
			Transactions struct {
				Edges []struct {
					Node txNode `json:"node"`
				} `json:"edges"`
			} `json:"transactions"`
		} `json:"data"`
	}

	if err := c.postGraphQL(ctx, query, map[string]any{"hash": strings.ToLower(txHash)}, &gqlResp); err != nil {
		return nil, err
	}
	if len(gqlResp.Data.Transactions.Edges) == 0 {
		// JSON-RPC returns null if not found
		jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
	}
	t := gqlResp.Data.Transactions.Edges[0].Node

	out := map[string]any{
		"hash":             toLowerHex(t.Hash),
		"nonce":            toHexU64(t.Nonce),
		"blockHash":        toLowerHex(t.BlockHash),
		"blockNumber":      toHexU64(t.BlockNumber),
		"transactionIndex": toHexU64(t.TransactionIndex),
		"from":             toLowerHex(t.From),
		"to":               nilIfEmptyHex(t.To),
		"value":            toHexBigFromString(t.Value),
		"gas":              toHexU64(t.Gas),
		"gasPrice":         toHexBigFromString(t.GasPrice),
		"input":            ensure0x(t.Input),
		// ECDSA fields
		"r": ensure0x(t.R),
		"s": ensure0x(t.S),
		"v": toHexU64(t.V),
		// Optional fields (commonly present but may be null in some archives)
		"type":                 nil, // fill if archive exposes it
		"chainId":              nil, // fill if available
		"maxFeePerGas":         nil,
		"maxPriorityFeePerGas": nil,
		// YParity (EIP-1559/2930) could be derived from v in some contexts; leaving null here.
	}

	jrr, _ := common.NewJsonRpcResponse(jrq.ID, out, nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// ---------- eth_getTransactionReceipt ----------

func (c *SubsquidClient) handleEthGetTransactionReceipt(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	hashI, err := jrq.PeekByPath(0)
	if err != nil || hashI == nil {
		return nil, fmt.Errorf("missing tx hash")
	}
	txHash := toLowerHex(hashI.(string))

	// GraphQL: single transaction joined with receipt-ish fields + logs
	query := `
query($hash: String!) {
  transactions: transactions(where: { hash_eq: $hash }, limit: 1) {
    edges {
      node {
        hash
        from
        to
        contractAddress
        status
        blockNumber
        blockHash
        transactionIndex
        cumulativeGasUsed
        gasUsed
        effectiveGasPrice
        logs {
          address
          data
          topics
          index
          blockNumber
          blockHash
          transactionIndex
          transactionHash
        }
      }
    }
  }
}
`

	type logNode struct {
		Address          string   `json:"address"`
		Data             string   `json:"data"`
		Topics           []string `json:"topics"`
		Index            uint64   `json:"index"`
		BlockNumber      uint64   `json:"blockNumber"`
		BlockHash        string   `json:"blockHash"`
		TransactionIndex uint64   `json:"transactionIndex"`
		TransactionHash  string   `json:"transactionHash"`
	}
	type recNode struct {
		Hash              string    `json:"hash"`
		From              string    `json:"from"`
		To                *string   `json:"to"`
		ContractAddress   *string   `json:"contractAddress"`
		Status            any       `json:"status"` // some archives expose bool, others 0/1
		BlockNumber       uint64    `json:"blockNumber"`
		BlockHash         string    `json:"blockHash"`
		TransactionIndex  uint64    `json:"transactionIndex"`
		CumulativeGasUsed uint64    `json:"cumulativeGasUsed"`
		GasUsed           uint64    `json:"gasUsed"`
		EffectiveGasPrice string    `json:"effectiveGasPrice"` // decimal as string (often)
		Logs              []logNode `json:"logs"`
	}
	var gqlResp struct {
		Data struct {
			Transactions struct {
				Edges []struct {
					Node recNode `json:"node"`
				} `json:"edges"`
			} `json:"transactions"`
		} `json:"data"`
	}

	if err := c.postGraphQL(ctx, query, map[string]any{"hash": strings.ToLower(txHash)}, &gqlResp); err != nil {
		return nil, err
	}
	if len(gqlResp.Data.Transactions.Edges) == 0 {
		// JSON-RPC returns null if not found
		jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
	}
	rx := gqlResp.Data.Transactions.Edges[0].Node

	// Map logs
	outLogs := make([]map[string]any, 0, len(rx.Logs))
	for _, lg := range rx.Logs {
		outLogs = append(outLogs, map[string]any{
			"address":          toLowerHex(lg.Address),
			"topics":           toLowerHexSlice(lg.Topics),
			"data":             ensure0x(lg.Data),
			"blockNumber":      toHexU64(lg.BlockNumber),
			"blockHash":        toLowerHex(lg.BlockHash),
			"transactionHash":  toLowerHex(lg.TransactionHash),
			"transactionIndex": toHexU64(lg.TransactionIndex),
			"logIndex":         toHexU64(lg.Index),
			"removed":          false,
		})
	}

	statusHex := "0x0"
	switch v := rx.Status.(type) {
	case bool:
		if v {
			statusHex = "0x1"
		}
	case float64:
		if uint64(v) != 0 {
			statusHex = "0x1"
		}
	case int:
		if v != 0 {
			statusHex = "0x1"
		}
	case string:
		// accept "1"/"0"
		if v == "1" {
			statusHex = "0x1"
		}
	}

	out := map[string]any{
		"transactionHash":   toLowerHex(rx.Hash),
		"transactionIndex":  toHexU64(rx.TransactionIndex),
		"blockHash":         toLowerHex(rx.BlockHash),
		"blockNumber":       toHexU64(rx.BlockNumber),
		"from":              toLowerHex(rx.From),
		"to":                nilIfEmptyHex(rx.To),
		"cumulativeGasUsed": toHexU64(rx.CumulativeGasUsed),
		"gasUsed":           toHexU64(rx.GasUsed),
		"contractAddress":   nilIfEmptyHex(rx.ContractAddress),
		"logs":              outLogs,
		"logsBloom":         nil, // fill if archive exposes it
		"status":            statusHex,
		"effectiveGasPrice": toHexBigFromString(rx.EffectiveGasPrice),
		"type":              nil, // optional
	}

	jrr, _ := common.NewJsonRpcResponse(jrq.ID, out, nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// ---------- eth_getBlockByHash / eth_getBlockByNumber ----------

func (c *SubsquidClient) handleEthGetBlock(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("parse json-rpc request: %w", err)
	}
	method, _ := req.Method()

	var (
		byHash bool
	)
	switch method {
	case "eth_getBlockByHash":
		byHash = true
	case "eth_getBlockByNumber":
		byHash = false
	default:
		return nil, fmt.Errorf("unsupported block method: %s", method)
	}

	fullTx := false
	if fv, err := jrq.PeekByPath(1); err == nil && fv != nil {
		if b, ok := fv.(bool); ok {
			fullTx = b
		}
	}

	var where map[string]any = map[string]any{}
	if byHash {
		hashI, err := jrq.PeekByPath(0)
		if err != nil || hashI == nil {
			return nil, fmt.Errorf("missing block hash")
		}
		where["hash_eq"] = toLowerHex(hashI.(string))
	} else {
		refI, err := jrq.PeekByPath(0)
		if err != nil || refI == nil {
			return nil, fmt.Errorf("missing block number ref")
		}
		if n, ok := normalizeBlockRef(refI); ok {
			where["number_eq"] = int64(n)
		} else {
			// latest/earliest not supported here -> null
			jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
		}
	}

	// Two variants of selection set based on fullTx
	var txSel string
	if fullTx {
		txSel = `
        transactions(limit: 10000, orderBy: [INDEX_ASC]) {
          edges { node {
            hash
            from
            to
            value
            input
            nonce
            gas
            gasPrice
            transactionIndex
            r
            s
            v
          } }
        }`
	} else {
		txSel = `
        transactions(limit: 10000, orderBy: [INDEX_ASC]) {
          edges { node { hash } }
        }`
	}

	query := fmt.Sprintf(`
query($where: BlocksBoolExp!) {
  blocks: blocks(where: $where, limit: 1) {
    edges {
      node {
        number
        hash
        parentHash
        timestamp
        miner
        gasLimit
        gasUsed
        baseFeePerGas
        %s
      }
    }
  }
}
`, txSel)

	type txNode struct {
		Hash             string  `json:"hash"`
		From             string  `json:"from"`
		To               *string `json:"to"`
		Value            string  `json:"value"`
		Input            string  `json:"input"`
		Nonce            uint64  `json:"nonce"`
		Gas              uint64  `json:"gas"`
		GasPrice         string  `json:"gasPrice"`
		TransactionIndex uint64  `json:"transactionIndex"`
		R                string  `json:"r"`
		S                string  `json:"s"`
		V                uint64  `json:"v"`
	}
	type blkNode struct {
		Number        uint64 `json:"number"`
		Hash          string `json:"hash"`
		ParentHash    string `json:"parentHash"`
		Timestamp     uint64 `json:"timestamp"`
		Miner         string `json:"miner"`
		GasLimit      uint64 `json:"gasLimit"`
		GasUsed       uint64 `json:"gasUsed"`
		BaseFeePerGas string `json:"baseFeePerGas"`
		Transactions  struct {
			Edges []struct {
				Node txNode `json:"node"`
			} `json:"edges"`
		} `json:"transactions"`
	}
	var gqlResp struct {
		Data struct {
			Blocks struct {
				Edges []struct {
					Node blkNode `json:"node"`
				} `json:"edges"`
			} `json:"blocks"`
		} `json:"data"`
	}

	if err := c.postGraphQL(ctx, query, map[string]any{"where": where}, &gqlResp); err != nil {
		return nil, err
	}
	if len(gqlResp.Data.Blocks.Edges) == 0 {
		jrr, _ := common.NewJsonRpcResponse(jrq.ID, nil, nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
	}
	b := gqlResp.Data.Blocks.Edges[0].Node

	var txs any
	if fullTx {
		list := make([]map[string]any, 0, len(b.Transactions.Edges))
		for _, e := range b.Transactions.Edges {
			t := e.Node
			list = append(list, map[string]any{
				"hash":                 toLowerHex(t.Hash),
				"nonce":                toHexU64(t.Nonce),
				"blockHash":            toLowerHex(b.Hash),
				"blockNumber":          toHexU64(b.Number),
				"transactionIndex":     toHexU64(t.TransactionIndex),
				"from":                 toLowerHex(t.From),
				"to":                   nilIfEmptyHex(t.To),
				"value":                toHexBigFromString(t.Value),
				"gas":                  toHexU64(t.Gas),
				"gasPrice":             toHexBigFromString(t.GasPrice),
				"input":                ensure0x(t.Input),
				"r":                    ensure0x(t.R),
				"s":                    ensure0x(t.S),
				"v":                    toHexU64(t.V),
				"type":                 nil,
				"chainId":              nil,
				"maxFeePerGas":         nil,
				"maxPriorityFeePerGas": nil,
			})
		}
		txs = list
	} else {
		// Only hashes
		list := make([]string, 0, len(b.Transactions.Edges))
		for _, e := range b.Transactions.Edges {
			list = append(list, toLowerHex(e.Node.Hash))
		}
		txs = list
	}

	out := map[string]any{
		"number":                toHexU64(b.Number),
		"hash":                  toLowerHex(b.Hash),
		"parentHash":            toLowerHex(b.ParentHash),
		"nonce":                 nil,        // not usually in archives
		"sha3Uncles":            []string{}, // archives typically don’t expose uncles; return empty array
		"logsBloom":             nil,
		"transactionsRoot":      nil,
		"stateRoot":             nil,
		"miner":                 toLowerHex(b.Miner),
		"difficulty":            "0x0",
		"totalDifficulty":       nil,
		"extraData":             "0x",
		"size":                  nil, // optional
		"gasLimit":              toHexU64(b.GasLimit),
		"gasUsed":               toHexU64(b.GasUsed),
		"timestamp":             toHexU64(b.Timestamp),
		"transactions":          txs,
		"uncles":                []string{},
		"baseFeePerGas":         toHexBigFromString(b.BaseFeePerGas),
		"mixHash":               nil,
		"receiptsRoot":          nil,
		"withdrawalsRoot":       nil,
		"withdrawals":           nil,
		"blobGasUsed":           nil,
		"excessBlobGas":         nil,
		"parentBeaconBlockRoot": nil,
	}

	jrr, _ := common.NewJsonRpcResponse(jrq.ID, out, nil)
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

// Converts optional *string hex address to either lower-hex or nil.
func nilIfEmptyHex(p *string) any {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" || s == "0x" || s == "0x0" {
		return nil
	}
	return toLowerHex(s)
}

func toHexBigFromString(s string) string {
	// Accept already-hex
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		// normalize case/prefix
		if s == "0x" || s == "0x0" || s == "" {
			return "0x0"
		}
		return strings.ToLower(s)
	}
	// Treat empty as zero
	if strings.TrimSpace(s) == "" {
		return "0x0"
	}
	// Decimal -> hex
	var z big.Int
	if _, ok := z.SetString(s, 10); ok {
		if z.Sign() == 0 {
			return "0x0"
		}
		return "0x" + strings.ToLower(z.Text(16))
	}
	// Fallback: try parse as integer-ish float64 string
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		u := uint64(f)
		return toHexU64(u)
	}
	// Last resort
	return "0x0"
}
