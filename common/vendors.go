package common

import (
	"context"
	"net/http"

	"github.com/rs/zerolog"
)

// UpstreamClient is the thing UpstreamsRegistry uses to execute a single JSON-RPC request.
type UpstreamClient interface {
	Do(ctx context.Context, req *NormalizedRequest) (*JsonRpcResponse, error)
	Close() error
}

// If a Vendor implements this, the registry will ask the vendor to build a custom client
// instead of using the default EVM JSON-RPC client.
type VendorClientFactory interface {
	NewClient(ctx context.Context, logger *zerolog.Logger, upstream *UpstreamConfig) (UpstreamClient, error)
}

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error)
	SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(req *NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}
