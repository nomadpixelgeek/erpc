// thirdparty/subsquid/subsquid_client.go
package subsquid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// SubsquidClient is a vendor-specific upstream client that speaks GraphQL to Subsquid archives
// and adapts selected JSON-RPC methods to GraphQL queries.
type SubsquidClient struct {
	endpoint string
	chainHex string
	http     *http.Client
	logger   *zerolog.Logger
}

// NewSubsquidClient creates a Subsquid transport bound to a single archive endpoint.
func NewSubsquidClient(ctx context.Context, logger *zerolog.Logger, upsCfg *common.UpstreamConfig) (*SubsquidClient, error) {
	if upsCfg == nil {
		return nil, fmt.Errorf("subsquid: missing upstream config")
	}
	if upsCfg.Endpoint == "" {
		return nil, fmt.Errorf("subsquid: upstream %s requires endpoint", upsCfg.Id)
	}
	var chainHex string
	if upsCfg.Evm != nil && upsCfg.Evm.ChainId > 0 {
		chainHex = fmt.Sprintf("0x%x", upsCfg.Evm.ChainId)
	} else {
		// Fallback: unknown chain. We still construct the client so eth_getLogs can work,
		// but eth_chainId will return a generic error if asked.
		chainHex = ""
	}

	hc := &http.Client{
		Timeout: 30 * time.Second,
	}
	lg := logger
	if lg == nil {
		nop := zerolog.Nop()
		lg = &nop
	}

	return &SubsquidClient{
		endpoint: upsCfg.Endpoint, // expected: https://v2.archive.subsquid.io/network/<name>/graphql
		chainHex: chainHex,
		http:     hc,
		logger:   lg,
	}, nil
}

// Close implements optional cleanup (no-op here).
func (c *SubsquidClient) Close() error { return nil }

// Do is the single entrypoint called by eRPC to execute a JSON-RPC request against this upstream.
func (c *SubsquidClient) Do(ctx context.Context, req *common.JsonRpcRequest) (*common.JsonRpcResponse, error) {
	if req == nil {
		return nil, common.NewErrEndpointServerSideException(fmt.Errorf("subsquid: nil request"), nil, 0)
	}

	switch req.Method {
	case "eth_chainId":
		if c.chainHex == "" {
			return nil, common.NewErrEndpointServerSideException(fmt.Errorf("subsquid: chainId not configured for this upstream"), nil, 0)
		}
		return common.NewJsonRpcResponse(req.ID, c.chainHex, nil)

	case "eth_getLogs":
		// TODO: Implement GraphQL translation for Subsquid v2.
		// This stub purposefully returns an endpoint-scoped error (not NewErrUpstreamRequest).
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid: eth_getLogs translation not implemented yet"),
			nil,
			0,
		)

	// You can optionally support trace_* translations here in the future.
	default:
		// Mark as endpoint-scoped unsupported for all other methods.
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid: method %s not supported by vendor client", req.Method),
			nil,
			0,
		)
	}
}

// --- Below are helpers you’ll use once you wire GraphQL translation for eth_getLogs ---

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data,omitempty"`
	Errors json.RawMessage `json:"errors,omitempty"`
}

// postGraphQL executes a raw GraphQL request to the Subsquid endpoint and returns the decoded envelope.
// Use this from the future eth_getLogs translator.
func (c *SubsquidClient) postGraphQL(ctx context.Context, payload *gqlRequest) (*gqlResponse, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid json encode error: %v", err),
			map[string]interface{}{
				"error": err,
			},
			0,
		)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid http build error: %v", err),
			map[string]interface{}{
				"error": err,
			},
			0,
		)
	}
	httpReq.Header.Set("content-type", "application/json")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid http error: %v", err),
			map[string]interface{}{
				"error": err,
			},
			0,
		)
	}
	defer httpResp.Body.Close()

	body, readErr := ioReadAllLimit(httpResp.Body, 4<<20) // 4 MiB safety
	if readErr != nil {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid read error: %v", readErr),
			map[string]interface{}{
				"error": readErr,
			},
			httpResp.StatusCode,
		)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid non-2xx: %d %s", httpResp.StatusCode, string(body)),
			map[string]interface{}{
				"status": httpResp.StatusCode,
				"body":   string(body),
			},
			httpResp.StatusCode,
		)
	}

	var gql gqlResponse
	if err := json.Unmarshal(body, &gql); err != nil {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid json error: %v", err),
			map[string]interface{}{
				"error": err,
			},
			httpResp.StatusCode,
		)
	}
	if len(gql.Errors) > 0 {
		return nil, common.NewErrEndpointServerSideException(
			fmt.Errorf("subsquid graphql error: %s", string(gql.Errors)),
			nil,
			httpResp.StatusCode,
		)
	}

	return &gql, nil
}

// ioReadAllLimit is a tiny helper to avoid unbounded reads.
func ioReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(64 << 10) // 64 KiB initial
	n, err := buf.ReadFrom(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("read exceeds limit %d", limit)
	}
	return buf.Bytes(), nil
}
