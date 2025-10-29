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

var subsquidSlugToChainID = map[string]int64{
	"base-mainnet":      8453,
	"base-sepolia":      84532,
	"binance-mainnet":   56,
	"binance-testnet":   97,
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

func CreateSubsquidVendor() common.Vendor { return &subsquidVendor{} }
func (v *subsquidVendor) Name() string    { return "subsquid" }

// Keep permissive; used only for shorthand inference if ever needed.
func (v *subsquidVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if ups == nil {
		return false
	}
	ep := strings.ToLower(ups.Endpoint)
	return strings.HasPrefix(ep, "subsquid://") || strings.Contains(ep, ".subsquid.io")
}

func (v *subsquidVendor) GenerateConfigs(
	_ context.Context,
	_ *zerolog.Logger,
	upstream *common.UpstreamConfig,
	settings common.VendorSettings,
) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// Prefer settings.endpoint injected by buildProviderSettings()
	httpsEndpoint, _ := settings["endpoint"].(string)
	if httpsEndpoint == "" {
		// Fallback: if upstream.Endpoint used a custom scheme, convert it to https (do NOT set subsquid://)
		if upstream.Endpoint == "" {
			return nil, fmt.Errorf("subsquid vendor requires settings.endpoint (https://v2.archive.subsquid.io/...) or a usable upstream endpoint")
		}
		u, err := url.Parse(upstream.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream endpoint for subsquid: %w", err)
		}
		if strings.EqualFold(u.Scheme, "subsquid") {
			u.Scheme = "https"
			httpsEndpoint = u.String()
		} else {
			httpsEndpoint = u.String()
		}
	}

	// IMPORTANT: keep HTTPS; do NOT rewrite to subsquid:// (core transport doesn’t support it)
	upstream.Endpoint = httpsEndpoint
	upstream.Type = common.UpstreamTypeEvm
	return []*common.UpstreamConfig{upstream}, nil
}

func (v *subsquidVendor) SupportsNetwork(
	_ context.Context,
	_ *zerolog.Logger,
	settings common.VendorSettings,
	networkId string,
) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	cid, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}

	ep, _ := settings["endpoint"].(string)
	if ep == "" {
		return false, nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return false, nil
	}
	slug := strings.ToLower(path.Base(path.Clean(u.Path)))
	want, ok := subsquidSlugToChainID[slug]
	if !ok {
		return false, nil
	}
	return cid == want, nil
}

func (v *subsquidVendor) GetVendorSpecificErrorIfAny(
	_ *common.NormalizedRequest,
	_ *http.Response,
	_ interface{},
	_ map[string]interface{},
) error {
	return nil
}
