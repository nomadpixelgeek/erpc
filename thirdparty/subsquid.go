// thirdparty/subsquid.go
package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// Minimal, credential-less vendor for Subsquid public gateways.
// Works with shorthand endpoints like `subsquid://v2.archive.subsquid.io/network/base-mainnet`
// after convertUpstreamToProvider() sets settings["endpoint"] to the https URL.

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

	// Use the resolved https endpoint from buildProviderSettings().
	if upstream.Endpoint == "" {
		endpoint, _ := settings["endpoint"].(string)
		if endpoint == "" {
			return nil, fmt.Errorf("subsquid vendor requires settings.endpoint (https://v2.archive.subsquid.io/...)")
		}
		upstream.Endpoint = endpoint
	}

	// Upstream type is EVM; chainId should be determined by network config, not here.
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

// SupportsNetwork gates provider discovery per network.
// For simplicity, accept any EVM:* network. If you want to restrict, parse chainId.
func (v *subsquidVendor) SupportsNetwork(
	_ context.Context,
	_ *zerolog.Logger,
	_ common.VendorSettings,
	networkId string,
) (bool, error) {
	// Expect "evm:<chainId>"
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	// If you want to restrict to certain chains, uncomment:
	// cid, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	// if err != nil { return false, err }
	// switch cid {
	// case 8453, 56: // base, bsc
	//     return true, nil
	// default:
	//     return false, nil
	// }
	// Default: allow all EVM networks (provider/upstream selection will filter further).
	if _, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64); err != nil {
		return false, err
	}
	return true, nil
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
