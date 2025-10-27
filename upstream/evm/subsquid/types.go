package subsquid

// NOTE: These are intentionally minimal and aligned with Subsquid Network
// EVM Gateway v2 semantics (Router/Worker). The selector/response fields
// can be extended as needed.

type RouterHeightResp struct {
	Height uint64 `json:"height"`
}

type WorkerQuery struct {
	FromBlock uint64         `json:"fromBlock"`
	ToBlock   *uint64        `json:"toBlock,omitempty"`
	Fields    *FieldSelector `json:"fields,omitempty"`
	Logs      *LogsSelector  `json:"logs,omitempty"`
	Traces    *TracesSelector`json:"traces,omitempty"`
	// Transactions, StateDiffs can be added later
}

type FieldSelector struct {
	Header       *HeaderFields       `json:"header,omitempty"`
	Log          *LogFields          `json:"log,omitempty"`
	Trace        *TraceFields        `json:"trace,omitempty"`
	Transaction  *TransactionFields  `json:"transaction,omitempty"`
}

type HeaderFields struct {
	Number     bool `json:"number,omitempty"`
	Hash       bool `json:"hash,omitempty"`
	ParentHash bool `json:"parentHash,omitempty"`
	Timestamp  bool `json:"timestamp,omitempty"`
}

type LogFields struct {
	Address         bool `json:"address,omitempty"`
	Topics          bool `json:"topics,omitempty"`
	Data            bool `json:"data,omitempty"`
	BlockNumber     bool `json:"blockNumber,omitempty"`
	TransactionHash bool `json:"transactionHash,omitempty"`
	TxIndex         bool `json:"transactionIndex,omitempty"`
	LogIndex        bool `json:"logIndex,omitempty"`
}

type TraceFields struct {
	Type            bool `json:"type,omitempty"`
	From            bool `json:"from,omitempty"`
	To              bool `json:"to,omitempty"`
	Value           bool `json:"value,omitempty"`
	Input           bool `json:"input,omitempty"`
	Output          bool `json:"output,omitempty"`
	BlockNumber     bool `json:"blockNumber,omitempty"`
	TransactionHash bool `json:"transactionHash,omitempty"`
	TraceAddress    bool `json:"traceAddress,omitempty"`
	Error           bool `json:"error,omitempty"`
	// Add when needed: substate, callType, etc.
}

type TransactionFields struct {
	Hash        bool `json:"hash,omitempty"`
	BlockNumber bool `json:"blockNumber,omitempty"`
	From        bool `json:"from,omitempty"`
	To          bool `json:"to,omitempty"`
	Index       bool `json:"transactionIndex,omitempty"`
}

// LogsSelector mirrors eth_getLogs filter semantics.
type LogsSelector struct {
	// single or multiple addresses; lower-cased
	Address []string `json:"address,omitempty"`
	// topics: null/wildcards map to absence of that topic key
	Topic0 []string `json:"topic0,omitempty"`
	Topic1 []string `json:"topic1,omitempty"`
	Topic2 []string `json:"topic2,omitempty"`
	Topic3 []string `json:"topic3,omitempty"`
}

// TracesSelector – extend as needed (from/to/codeHash/sighash filters, etc.)
type TracesSelector struct {
	From       []string `json:"from,omitempty"`
	To         []string `json:"to,omitempty"`
	Sighash    []string `json:"sighash,omitempty"`
	Type       []string `json:"type,omitempty"` // call/create/selfdestruct, etc.
	// Add block/tx constraints via FromBlock/ToBlock at top-level WorkerQuery
}

// WorkerResponse is a page of blocks; each entry has header, optionally logs/traces/etc.
type WorkerResponse struct {
	Items []BlockSlice `json:"items"`
}

type BlockSlice struct {
	Header       *Header        `json:"header,omitempty"`
	Logs         []Log          `json:"logs,omitempty"`
	Traces       []Trace        `json:"traces,omitempty"`
	Transactions []Transaction  `json:"transactions,omitempty"`
	// StateDiffs omitted for now
}

type Header struct {
	Number     uint64 `json:"number"`
	Hash       string `json:"hash"`
	ParentHash string `json:"parentHash"`
	Timestamp  uint64 `json:"timestamp,omitempty"`
}

type Log struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	Data            string   `json:"data"`
	BlockNumber     uint64   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	TransactionIndex uint32  `json:"transactionIndex"`
	LogIndex        uint32   `json:"logIndex"`
}

type Trace struct {
	Type            string   `json:"type,omitempty"`
	From            string   `json:"from,omitempty"`
	To              string   `json:"to,omitempty"`
	Value           string   `json:"value,omitempty"` // hex string per Subsquid
	Input           string   `json:"input,omitempty"`
	Output          string   `json:"output,omitempty"`
	BlockNumber     uint64   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	TraceAddress    []uint32 `json:"traceAddress,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type Transaction struct {
	Hash            string `json:"hash"`
	BlockNumber     uint64 `json:"blockNumber"`
	From            string `json:"from,omitempty"`
	To              string `json:"to,omitempty"`
	TransactionIndex uint32 `json:"transactionIndex,omitempty"`
}

// --- JSON-RPC plumbing ---

type JSONRPCRequest struct {
	Jsonrpc string           `json:"jsonrpc"`
	ID      interface{}      `json:"id"`
	Method  string           `json:"method"`
	Params  []any            `json:"params"`
}

type JSONRPCResponse struct {
	Jsonrpc string           `json:"jsonrpc"`
	ID      interface{}      `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *JSONRPCError    `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func ErrResp(id any, code int, msg string) *JSONRPCResponse {
	return &JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: msg},
	}
}
