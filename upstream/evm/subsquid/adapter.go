package subsquid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Upstream implements a minimal eRPC-compatible adapter.
// Replace the interface below with your project's actual EVM upstream interface.
type Upstream interface {
	Handle(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error)
	Capabilities() map[string]bool
}

type Adapter struct {
	client  *Client
	chainID uint64 // required by config (Base=8453, BSC=56)
	// tunables
	maxChunk uint64        // max block span per worker query
	timeout  time.Duration // per subrequest timeout
}

type Config struct {
	Gateway string
	ChainID uint64
	MaxChunk uint64
	Timeout  time.Duration
}

func NewAdapter(cfg Config) *Adapter {
	if cfg.MaxChunk == 0 {
		cfg.MaxChunk = 10_000
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Adapter{
		client:  NewClient(cfg.Gateway),
		chainID: cfg.ChainID,
		maxChunk: cfg.MaxChunk,
		timeout:  cfg.Timeout,
	}
}

func (a *Adapter) Capabilities() map[string]bool {
	// We explicitly advertise only methods we implement via Subsquid.
	return map[string]bool{
		"eth_blockNumber":          true, // finalized height
		"eth_getBlockByNumber":     true, // finalized only; full=false preferred
		"eth_getLogs":              true,
		"trace_filter":             true,
		"trace_block":              true,
		// You can add "trace_transaction" later if you implement hash-based scans.
	}
}

func (a *Adapter) Handle(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	switch req.Method {
	case "eth_blockNumber":
		return a.handleBlockNumber(ctx, req)
	case "eth_getBlockByNumber":
		return a.handleGetBlockByNumber(ctx, req)
	case "eth_getLogs":
		return a.handleGetLogs(ctx, req)
	case "trace_filter":
		return a.handleTraceFilter(ctx, req)
	case "trace_block":
		return a.handleTraceBlock(ctx, req)
	default:
		// Everything else is intentionally unsupported to allow eRPC to route/fallback
		return nil, ErrUnsupported
	}
}

// --- handlers ---

func (a *Adapter) handleBlockNumber(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	h, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}
	return &JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      req.ID,
		Result:  u64Hex(h),
	}, nil
}

func (a *Adapter) handleGetBlockByNumber(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	// params: [blockTag, fullTx bool]
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing block param"), nil
	}
	var blockTag string
	var wantFull bool
	if s, ok := req.Params[0].(string); ok {
		blockTag = s
	} else {
		return ErrResp(req.ID, -32602, "invalid block param"), nil
	}
	if len(req.Params) > 1 {
		if b, ok := req.Params[1].(bool); ok {
			wantFull = b
		}
	}
	if wantFull {
		// Subsquid header-only; return unsupported so eRPC can route to JSON-RPC.
		return nil, ErrUnsupported
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// Resolve height for "latest/finalized"
	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}

	var n uint64
	switch blockTag {
	case "latest", "finalized", "safe":
		n = height
	default:
		n, err = hexU64(blockTag)
		if err != nil {
			return ErrResp(req.ID, -32602, "invalid block number"), nil
		}
		if n > height {
			// Outside finalized range; ask a different upstream
			return nil, ErrUnsupported
		}
	}

	worker, err := a.client.PickWorker(ctx, n)
	if err != nil {
		return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil
	}

	q := &WorkerQuery{
		FromBlock: n,
		ToBlock:   &n,
		Fields: &FieldSelector{
			Header: &HeaderFields{Number: true, Hash: true, ParentHash: true, Timestamp: true},
		},
	}

	out, err := a.client.QueryWorker(ctx, worker, q)
	if err != nil {
		return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
	}
	if len(out.Items) == 0 || out.Items[0].Header == nil {
		// Not found — return null per JSON-RPC spec for missing block
		return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: nil}, nil
	}

	h := out.Items[0].Header
	// Build a minimal geth-like block object without tx bodies
	block := map[string]any{
		"number":     u64Hex(h.Number),
		"hash":       h.Hash,
		"parentHash": h.ParentHash,
		"timestamp":  u64Hex(h.Timestamp),
		// set fields commonly expected even if empty
		"transactions": []any{},
		"uncles":       []any{},
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: block}, nil
}

func (a *Adapter) handleGetLogs(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	// params: [filter]
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing filter"), nil
	}
	filterBytes, _ := json.Marshal(req.Params[0])
	var f struct {
		FromBlock *string     `json:"fromBlock"`
		ToBlock   *string     `json:"toBlock"`
		BlockHash *string     `json:"blockHash"`
		Address   any         `json:"address"` // string or []string
		Topics    []any       `json:"topics"`  // entries can be string|null|[]string
	}
	if err := json.Unmarshal(filterBytes, &f); err != nil {
		return ErrResp(req.ID, -32602, "invalid filter"), nil
	}
	// We only support from/to ranges (finalized). blockHash exact not supported here.
	if f.BlockHash != nil {
		// Let other upstreams handle blockHash-specific queries
		return nil, ErrUnsupported
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}

	// Resolve range
	from := uint64(0)
	to := height
	if f.FromBlock != nil {
		switch *f.FromBlock {
		case "latest", "finalized", "safe":
			from = height
		default:
			if n, err := hexU64(*f.FromBlock); err == nil {
				from = n
			}
		}
	}
	if f.ToBlock != nil {
		switch *f.ToBlock {
		case "latest", "finalized", "safe":
			to = height
		default:
			if n, err := hexU64(*f.ToBlock); err == nil {
				to = n
			}
		}
	}
	if to > height {
		to = height
	}
	if from > to {
		return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: []any{}}, nil
	}

	// Build logs selector
	var addresses []string
	switch v := f.Address.(type) {
	case string:
		if v != "" {
			addresses = []string{strings.ToLower(v)}
		}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				addresses = append(addresses, strings.ToLower(s))
			}
		}
	}
	getTopic := func(i int) []string {
		if i >= len(f.Topics) || f.Topics[i] == nil {
			return nil
		}
		switch tv := f.Topics[i].(type) {
		case string:
			return []string{tv}
		case []any:
			out := make([]string, 0, len(tv))
			for _, z := range tv {
				if s, ok := z.(string); ok {
					out = append(out, s)
				}
			}
			return out
		default:
			return nil
		}
	}

	selector := &LogsSelector{
		Address: addresses,
		Topic0:  getTopic(0),
		Topic1:  getTopic(1),
		Topic2:  getTopic(2),
		Topic3:  getTopic(3),
	}

	fields := &FieldSelector{
		Header: &HeaderFields{Number: true, Hash: true},
		Log: &LogFields{
			Address: true, Topics: true, Data: true,
			BlockNumber: true, TransactionHash: true,
			TxIndex: true, LogIndex: true,
		},
	}

	var results []map[string]any
	start := from
	for start <= to {
		end := start + a.maxChunk - 1
		if end > to {
			end = to
		}
		worker, err := a.client.PickWorker(ctx, start)
		if err != nil {
			return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil
		}
		q := &WorkerQuery{
			FromBlock: start,
			ToBlock:   &end,
			Fields:    fields,
			Logs:      selector,
		}
		page, err := a.client.QueryWorker(ctx, worker, q)
		if err != nil {
			return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
		}
		for _, it := range page.Items {
			for _, lg := range it.Logs {
				results = append(results, map[string]any{
					"address":          lg.Address,
					"topics":           lg.Topics,
					"data":             lg.Data,
					"blockNumber":      u64Hex(lg.BlockNumber),
					"transactionHash":  lg.TransactionHash,
					"transactionIndex": fmt.Sprintf("0x%x", lg.TransactionIndex),
					"logIndex":         fmt.Sprintf("0x%x", lg.LogIndex),
					"blockHash":        it.Header.Hash, // available from header
					"removed":          false,          // finalized history
				})
			}
		}
		if end == to {
			break
		}
		start = end + 1
	}

	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: results}, nil
}

func (a *Adapter) handleTraceFilter(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	// Minimal parity-compatible subset: { fromBlock, toBlock, fromAddress, toAddress }
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing filter"), nil
	}
	raw, _ := json.Marshal(req.Params[0])
	var f struct {
		FromBlock *string   `json:"fromBlock"`
		ToBlock   *string   `json:"toBlock"`
		FromAddrs []string  `json:"fromAddress"`
		ToAddrs   []string  `json:"toAddress"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return ErrResp(req.ID, -32602, "invalid filter"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}
	from := uint64(0)
	to := height
	if f.FromBlock != nil {
		if n, err := a.resolveBlockTag(*f.FromBlock, height); err == nil {
			from = n
		}
	}
	if f.ToBlock != nil {
		if n, err := a.resolveBlockTag(*f.ToBlock, height); err == nil {
			to = n
		}
	}
	if from > to {
		return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: []any{}}, nil
	}

	fields := &FieldSelector{
		Trace: &TraceFields{
			Type: true, From: true, To: true, Value: true, Input: true, Output: true,
			BlockNumber: true, TransactionHash: true, TraceAddress: true, Error: true,
		},
	}

	sel := &TracesSelector{
		From: toLowerAll(f.FromAddrs),
		To:   toLowerAll(f.ToAddrs),
	}

	var results []map[string]any
	start := from
	for start <= to {
		end := start + a.maxChunk - 1
		if end > to {
			end = to
		}
		worker, err := a.client.PickWorker(ctx, start)
		if err != nil {
			return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil
		}
		q := &WorkerQuery{
			FromBlock: start,
			ToBlock:   &end,
			Fields:    fields,
			Traces:    sel,
		}
		page, err := a.client.QueryWorker(ctx, worker, q)
		if err != nil {
			return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
		}
		for _, it := range page.Items {
			for _, tr := range it.Traces {
				// Parity trace format (basic "call/create" traces)
				results = append(results, map[string]any{
					"action": map[string]any{
						"callType": nil, // can be filled if you include it in TraceFields
						"from":     tr.From,
						"to":       tr.To,
						"gas":      nil, // not provided by basic fields
						"input":    tr.Input,
						"value":    tr.Value,
					},
					"blockHash":        it.Header.Hash,
					"blockNumber":      tr.BlockNumber,
					"result": map[string]any{
						"gasUsed": nil,
						"output":  tr.Output,
					},
					"subtraces": len(tr.TraceAddress),
					"traceAddress": tr.TraceAddress,
					"transactionHash": tr.TransactionHash,
					"type": strings.ToLower(tr.Type),
					"error": tr.Error,
				})
			}
		}
		if end == to {
			break
		}
		start = end + 1
	}

	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: results}, nil
}

func (a *Adapter) handleTraceBlock(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	// params: [blockTag(hex)]
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing block param"), nil
	}
	blockParam, _ := req.Params[0].(string)

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}

	n, err := a.resolveBlockTag(blockParam, height)
	if err != nil {
		return ErrResp(req.ID, -32602, "invalid block param"), nil
	}
	if n > height {
		return nil, ErrUnsupported
	}

	worker, err := a.client.PickWorker(ctx, n)
	if err != nil {
		return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil
	}
	fields := &FieldSelector{
		Header: &HeaderFields{Hash: true, Number: true},
		Trace:  &TraceFields{
			Type: true, From: true, To: true, Value: true, Input: true, Output: true,
			BlockNumber: true, TransactionHash: true, TraceAddress: true, Error: true,
		},
	}
	q := &WorkerQuery{
		FromBlock: n,
		ToBlock:   &n,
		Fields:    fields,
		Traces:    &TracesSelector{},
	}
	page, err := a.client.QueryWorker(ctx, worker, q)
	if err != nil {
		return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
	}
	var res []map[string]any
	for _, it := range page.Items {
		for _, tr := range it.Traces {
			res = append(res, map[string]any{
				"action": map[string]any{
					"from":  tr.From,
					"to":    tr.To,
					"input": tr.Input,
					"value": tr.Value,
				},
				"blockHash":        it.Header.Hash,
				"blockNumber":      tr.BlockNumber,
				"result": map[string]any{"output": tr.Output},
				"subtraces":        len(tr.TraceAddress),
				"traceAddress":     tr.TraceAddress,
				"transactionHash":  tr.TransactionHash,
				"type":              strings.ToLower(tr.Type),
				"error":             tr.Error,
			})
		}
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: res}, nil
}

// --- helpers ---

func (a *Adapter) resolveBlockTag(tag string, height uint64) (uint64, error) {
	switch tag {
	case "latest", "finalized", "safe":
		return height, nil
	default:
		return hexU64(tag)
	}
}

func toLowerAll(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, s := range xs {
		if s == "" {
			continue
		}
		out = append(out, strings.ToLower(s))
	}
	return out
}

// Optional: small HTTP handler shim for direct testing
func (a *Adapter) HTTP() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			_ = json.NewEncoder(w).Encode(ErrResp(nil, -32700, "parse error"))
			return
		}
		resp, err := a.Handle(r.Context(), &req)
		if errors.Is(err, ErrUnsupported) {
			// For manual tests, show a clear error; in eRPC proper, this gets routed to another upstream.
			_ = json.NewEncoder(w).Encode(ErrResp(req.ID, -32012, "subsquid unsupported; route elsewhere"))
			return
		}
		if err != nil {
			_ = json.NewEncoder(w).Encode(ErrResp(req.ID, -32000, err.Error()))
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}
