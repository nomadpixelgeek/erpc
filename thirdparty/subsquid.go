// thirdparty/subsquid.go
package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// Map Subsquid "network slugs" -> EVM chain IDs.
// Fill this if more networks required to enabled.
//
// IMPORTANT: The slug must match the last path segment in the Subsquid URL,
// e.g. https://v2.archive.subsquid.io/network/base-mainnet -> "base-mainnet".
var subsquidSlugToChainID = map[string]int64{
	// Your immediate needs:
	"base-mainnet":    8453,
	"base-sepolia":    84532,
	"binance-mainnet": 56, // Subsquid docs use "binance-mainnet" (not "bsc-mainnet")
	"binance-testnet": 97,

	// A few common ones (keep or remove as you like):
	"ethereum-mainnet":  1,
	"ethereum-sepolia":  11155111,
	"arbitrum-one":      42161,
	"optimism-mainnet":  10,
	"polygon-mainnet":   137,
	"mantle-mainnet":    5000,
	"berachain-mainnet": 80094,
	"mode-mainnet":      34443,
	"sonic-mainnet":     146,
	"blast-l2-mainnet":  81457,
}

type subsquidVendor struct{}

// Factory used by VendorsRegistry.
func CreateSubsquidVendor() common.Vendor { return &subsquidVendor{} }

func (v *subsquidVendor) Name() string { return "subsquid" }

// OwnsUpstream allows the loader to infer the vendor when vendorName is omitted.
func (v *subsquidVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if ups == nil {
		return false
	}
	ep := strings.ToLower(ups.Endpoint)
	return strings.HasPrefix(ep, "subsquid://") || strings.Contains(ep, ".subsquid.io")
}

// GenerateConfigs produces final upstream(s) from provider settings.
// Expect settings["endpoint"] to be an https URL built in buildProviderSettings().
func (v *subsquidVendor) GenerateConfigs(
	_ context.Context,
	_ *zerolog.Logger,
	upstream *common.UpstreamConfig,
	settings common.VendorSettings,
) ([]*common.UpstreamConfig, error) {
	// Ensure JSON-RPC block exists
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// Use the resolved https endpoint from buildProviderSettings(), but DO NOT
	// set it as an https:// upstream endpoint. Keep the custom scheme so the
	// UpstreamsRegistry selects the Subsquid adapter.
	endpoint, _ := settings["endpoint"].(string)
	if endpoint == "" {
		return nil, fmt.Errorf("subsquid vendor requires settings.endpoint (https://v2.archive.subsquid.io/...)")
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid subsquid settings.endpoint: %w", err)
	}

	// Preserve the vendor-specific scheme on the upstream so the adapter is used.
	// Example: subsquid://v2.archive.subsquid.io/network/base-mainnet
	upstream.Endpoint = "subsquid://" + u.Host
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		upstream.Endpoint += "/" + p
	}
	if q := u.RawQuery; q != "" {
		upstream.Endpoint += "?" + q
	}

	// Mark as EVM; chainId will be taken from the Network (and already gated with SupportsNetwork).
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

// SupportsNetwork attaches this provider only to the Network matching its slug-implied chainId.
func (v *subsquidVendor) SupportsNetwork(
	_ context.Context,
	_ *zerolog.Logger,
	settings common.VendorSettings,
	networkId string,
) (bool, error) {
	// must be "evm:<chainId>"
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	cid, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	// settings["endpoint"] is something like:
	//   https://v2.archive.subsquid.io/network/base-mainnet
	endpoint, _ := settings["endpoint"].(string)
	if endpoint == "" {
		// If settings are missing, be conservative.
		return false, nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return false, err
	}
	// slug = last segment after /network/
	// path.Clean to normalize, then take base
	slug := path.Base(path.Clean(u.Path))

	want, ok := subsquidSlugToChainID[strings.ToLower(slug)]
	if !ok {
		// Unknown slug; don't attach to any network by default
		return false, nil
	}
	return cid == want, nil
}

// No vendor-specific error mapping required for public Subsquid gateways.
func (v *subsquidVendor) GetVendorSpecificErrorIfAny(
	_ *common.NormalizedRequest,
	_ *http.Response,
	_ interface{},
	_ map[string]interface{},
) error {
	return nil
}
