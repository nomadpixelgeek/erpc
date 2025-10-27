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

// Adapter is the eRPC upstream shim for Subsquid Network.
type Adapter struct {
	client   *Client
	chainID  uint64
	maxChunk uint64
	timeout  time.Duration
}

type Config struct {
	Gateway  string
	ChainID  uint64
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
		client:   NewClient(cfg.Gateway),
		chainID:  cfg.ChainID,
		maxChunk: cfg.MaxChunk,
		timeout:  cfg.Timeout,
	}
}

func (a *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		"eth_blockNumber":      true,
		"eth_getBlockByNumber": true,
		"eth_getLogs":          true,
		"trace_filter":         true,
		"trace_block":          true,
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
		return nil, ErrUnsupported
	}
}

func (a *Adapter) handleBlockNumber(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout); defer cancel()
	h, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: u64Hex(h)}, nil
}

func (a *Adapter) handleGetBlockByNumber(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing block param"), nil
	}
	var tag string
	var full bool
	if s, ok := req.Params[0].(string); ok {
		tag = s
	} else {
		return ErrResp(req.ID, -32602, "invalid block param"), nil
	}
	if len(req.Params) > 1 {
		if b, ok := req.Params[1].(bool); ok {
			full = b
		}
	}
	if full {
		return nil, ErrUnsupported // header-only here; route full blocks elsewhere
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout); defer cancel()
	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}
	n, err := a.resolveBlockTag(tag, height)
	if err != nil || n > height {
		return nil, ErrUnsupported
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
	page, err := a.client.QueryWorker(ctx, worker, q)
	if err != nil {
		return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
	}
	if len(page.Items) == 0 || page.Items[0].Header == nil {
		return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: nil}, nil
	}
	h := page.Items[0].Header
	block := map[string]any{
		"number":     u64Hex(h.Number),
		"hash":       h.Hash,
		"parentHash": h.ParentHash,
		"timestamp":  u64Hex(h.Timestamp),
		"transactions": []any{},
		"uncles":       []any{},
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: block}, nil
}

func (a *Adapter) handleGetLogs(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing filter"), nil
	}
	raw, _ := json.Marshal(req.Params[0])
	var f struct {
		FromBlock *string `json:"fromBlock"`
		ToBlock   *string `json:"toBlock"`
		BlockHash *string `json:"blockHash"`
		Address   any     `json:"address"`
		Topics    []any   `json:"topics"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return ErrResp(req.ID, -32602, "invalid filter"), nil
	}
	if f.BlockHash != nil {
		return nil, ErrUnsupported // let JSON-RPC handle exact-block queries
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout); defer cancel()
	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}

	from := uint64(0)
	to := height
	if f.FromBlock != nil {
		if n, err := a.resolveBlockTag(*f.FromBlock, height); err == nil { from = n }
	}
	if f.ToBlock != nil {
		if n, err := a.resolveBlockTag(*f.ToBlock, height); err == nil { to = n }
	}
	if to > height { to = height }
	if from > to { return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: []any{}}, nil }

	var addresses []string
	switch v := f.Address.(type) {
	case string:
		if v != "" { addresses = []string{strings.ToLower(v)} }
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				addresses = append(addresses, strings.ToLower(s))
			}
		}
	}
	getTopic := func(i int) []string {
		if i >= len(f.Topics) || f.Topics[i] == nil { return nil }
		switch tv := f.Topics[i].(type) {
		case string:
			return []string{tv}
		case []any:
			out := make([]string, 0, len(tv))
			for _, z := range tv { if s, ok := z.(string); ok { out = append(out, s) } }
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
			TransactionIndex: true, LogIndex: true,
		},
	}

	var out []map[string]any
	for start := from; start <= to; {
		end := start + a.maxChunk - 1
		if end > to { end = to }

		worker, err := a.client.PickWorker(ctx, start)
		if err != nil {
			return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil
		}
		page, err := a.client.QueryWorker(ctx, worker, &WorkerQuery{
			FromBlock: start, ToBlock: &end, Fields: fields, Logs: selector,
		})
		if err != nil {
			return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
		}

		for _, it := range page.Items {
			for _, lg := range it.Logs {
				out = append(out, map[string]any{
					"address":          lg.Address,
					"topics":           lg.Topics,
					"data":             lg.Data,
					"blockNumber":      u64Hex(lg.BlockNumber),
					"transactionHash":  lg.TransactionHash,
					"transactionIndex": fmt.Sprintf("0x%x", lg.TransactionIndex),
					"logIndex":         fmt.Sprintf("0x%x", lg.LogIndex),
					"blockHash":        it.Header.Hash,
					"removed":          false,
				})
			}
		}

		if end == to { break }
		start = end + 1
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: out}, nil
}

func (a *Adapter) handleTraceFilter(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if len(req.Params) < 1 {
		return ErrResp(req.ID, -32602, "missing filter"), nil
	}
	raw, _ := json.Marshal(req.Params[0])
	var f struct {
		FromBlock *string  `json:"fromBlock"`
		ToBlock   *string  `json:"toBlock"`
		FromAddr  []string `json:"fromAddress"`
		ToAddr    []string `json:"toAddress"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return ErrResp(req.ID, -32602, "invalid filter"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout); defer cancel()
	height, err := a.client.GetHeight(ctx)
	if err != nil {
		return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil
	}

	from := uint64(0)
	to := height
	if f.FromBlock != nil {
		if n, err := a.resolveBlockTag(*f.FromBlock, height); err == nil { from = n }
	}
	if f.ToBlock != nil {
		if n, err := a.resolveBlockTag(*f.ToBlock, height); err == nil { to = n }
	}
	if from > to { return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: []any{}}, nil }

	fields := &FieldSelector{
		Header: &HeaderFields{Hash: true, Number: true},
		Trace: &TraceFields{
			Type: true, From: true, To: true, Value: true, Input: true, Output: true,
			BlockNumber: true, TransactionHash: true, TraceAddress: true, Error: true,
		},
	}
	sel := &TracesSelector{From: toLowerAll(f.FromAddr), To: toLowerAll(f.ToAddr)}

	var out []map[string]any
	for start := from; start <= to; {
		end := start + a.maxChunk - 1
		if end > to { end = to }

		worker, err := a.client.PickWorker(ctx, start)
		if err != nil {
			return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil
		}
		page, err := a.client.QueryWorker(ctx, worker, &WorkerQuery{
			FromBlock: start, ToBlock: &end, Fields: fields, Traces: sel,
		})
		if err != nil {
			return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil
		}

		for _, it := range page.Items {
			for _, tr := range it.Traces {
				out = append(out, map[string]any{
					"action": map[string]any{
						"from":  tr.From,
						"to":    tr.To,
						"input": tr.Input,
						"value": tr.Value,
					},
					"blockHash":        it.Header.Hash,
					"blockNumber":      tr.BlockNumber,
					"result":           map[string]any{"output": tr.Output},
					"subtraces":        len(tr.TraceAddress),
					"traceAddress":     tr.TraceAddress,
					"transactionHash":  tr.TransactionHash,
					"type":             strings.ToLower(tr.Type),
					"error":            tr.Error,
				})
			}
		}

		if end == to { break }
		start = end + 1
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: out}, nil
}

func (a *Adapter) handleTraceBlock(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if len(req.Params) < 1 { return ErrResp(req.ID, -32602, "missing block param"), nil }
	tag, _ := req.Params[0].(string)

	ctx, cancel := context.WithTimeout(ctx, a.timeout); defer cancel()
	height, err := a.client.GetHeight(ctx)
	if err != nil { return ErrResp(req.ID, -32001, "subsquid router height: "+err.Error()), nil }

	n, err := a.resolveBlockTag(tag, height)
	if err != nil || n > height { return nil, ErrUnsupported }

	worker, err := a.client.PickWorker(ctx, n)
	if err != nil { return ErrResp(req.ID, -32001, "pick worker: "+err.Error()), nil }

	fields := &FieldSelector{
		Header: &HeaderFields{Hash: true, Number: true},
		Trace: &TraceFields{
			Type: true, From: true, To: true, Value: true, Input: true, Output: true,
			BlockNumber: true, TransactionHash: true, TraceAddress: true, Error: true,
		},
	}
	page, err := a.client.QueryWorker(ctx, worker, &WorkerQuery{
		FromBlock: n, ToBlock: &n, Fields: fields, Traces: &TracesSelector{},
	})
	if err != nil { return ErrResp(req.ID, -32001, "worker query: "+err.Error()), nil }

	var out []map[string]any
	for _, it := range page.Items {
		for _, tr := range it.Traces {
			out = append(out, map[string]any{
				"action": map[string]any{"from": tr.From, "to": tr.To, "input": tr.Input, "value": tr.Value},
				"blockHash":        it.Header.Hash,
				"blockNumber":      tr.BlockNumber,
				"result":           map[string]any{"output": tr.Output},
				"subtraces":        len(tr.TraceAddress),
				"traceAddress":     tr.TraceAddress,
				"transactionHash":  tr.TransactionHash,
				"type":             strings.ToLower(tr.Type),
				"error":            tr.Error,
			})
		}
	}
	return &JSONRPCResponse{Jsonrpc: "2.0", ID: req.ID, Result: out}, nil
}

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
		if s != "" { out = append(out, strings.ToLower(s)) }
	}
	return out
}

// Handy local test server (optional)
func (a *Adapter) HTTP() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			_ = json.NewEncoder(w).Encode(ErrResp(nil, -32700, "parse error")); return
		}
		resp, err := a.Handle(r.Context(), &req)
		if errors.Is(err, ErrUnsupported) {
			_ = json.NewEncoder(w).Encode(ErrResp(req.ID, -32012, "subsquid unsupported; route elsewhere")); return
		}
		if err != nil {
			_ = json.NewEncoder(w).Encode(ErrResp(req.ID, -32000, err.Error())); return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}
