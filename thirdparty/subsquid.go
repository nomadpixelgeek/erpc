package thirdparty

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

// Make sure this type implements common.Vendor.
// If your common.Vendor interface differs slightly, mirror the methods used
// in your simplest vendor (e.g., phony.go) and adjust signatures accordingly.
type subsquidVendor struct {
	common.Vendor
}

func CreateSubsquidVendor() common.Vendor { return &subsquidVendor{} }

func (v *subsquidVendor) Name() string { return "subsquid" }

// OwnsUpstream lets users omit vendorName if the endpoint scheme is subsquid://
func (v *subsquidVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if ups == nil {
		return false
	}
	if ups.VendorName == v.Name() {
		return true
	}
	return strings.HasPrefix(strings.ToLower(ups.Endpoint), "subsquid://")
}

// If your Vendor interface requires parsing settings, accept anything.
func (v *subsquidVendor) ParseSettings(raw any) error {
	// No credentials/settings needed for public Subsquid Network gateways.
	return nil
}

// If your Vendor interface supports request auth/mutation, do nothing.
func (v *subsquidVendor) ApplyAuth(_ *http.Request, _ *common.UpstreamConfig) {}

// If your Vendor interface has other no-op hooks (e.g., Headers(), Params(), etc.),
// implement them here mirroring the simplest existing vendor in this repo.
