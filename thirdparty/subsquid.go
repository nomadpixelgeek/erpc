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

func (v *subsquidVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if ups == nil {
		return false
	}
	ep := strings.ToLower(ups.Endpoint)
	return strings.HasPrefix(ep, "subsquid://") || strings.Contains(ep, ".subsquid.io")
}

// GenerateConfigs produces final upstream(s) from provider settings.
// Expect settings["endpoint"] to be an https URL built in buildProviderSettings().
// IMPORTANT: This works even if settings["endpoint"] is missing.
// It is reconstructed from upstream.Endpoint when needed.
func (v *subsquidVendor) GenerateConfigs(
	_ context.Context,
	_ *zerolog.Logger,
	upstream *common.UpstreamConfig,
	settings common.VendorSettings,
) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	httpsEndpoint, _ := settings["endpoint"].(string)

	if httpsEndpoint == "" {
		// Fallback path: convert subsquid://host/path?query → https://host/path?query
		if upstream.Endpoint == "" {
			return nil, fmt.Errorf("subsquid vendor requires settings.endpoint (https://v2.archive.subsquid.io/...) or a subsquid:// upstream endpoint")
		}
		u, err := url.Parse(upstream.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream endpoint for subsquid: %w", err)
		}
		if !strings.EqualFold(u.Scheme, "subsquid") {
			httpsEndpoint = u.String()
		} else {
			u.Scheme = "https"
			httpsEndpoint = u.String()
		}
	}

	// Normalize upstream.Endpoint to subsquid:// so the Subsquid adapter is used.
	u, err := url.Parse(httpsEndpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid subsquid settings.endpoint: %w", err)
	}
	upstream.Endpoint = "subsquid://" + u.Host
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		upstream.Endpoint += "/" + p
	}
	if u.RawQuery != "" {
		upstream.Endpoint += "?" + u.RawQuery
	}

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

	// Prefer settings.endpoint, but also recover from upstream endpoint if needed.
	httpsEndpoint, _ := settings["endpoint"].(string)

	var slug string
	if httpsEndpoint != "" {
		if u, err := url.Parse(httpsEndpoint); err == nil {
			slug = strings.ToLower(path.Base(path.Clean(u.Path)))
		}
	}
	// If settings were empty or unparsable, we can’t easily read the upstream here,
	// because SupportsNetwork doesn’t receive it. Be conservative in that case.
	if slug == "" {
		return false, nil
	}

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
