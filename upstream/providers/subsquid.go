package providers

import (
	"net/url"
	"strings"
	"time"

	ss "github.com/erpc/erpc/upstream/evm/subsquid"
)

type SubsquidProviderConfig struct {
	Endpoint string `yaml:"endpoint"` // subsquid://v2.archive.subsquid.io/network/base-mainnet
	EVM struct {
		ChainID uint64 `yaml:"chainId"`
	} `yaml:"evm"`
	MaxChunk uint64 `yaml:"maxChunk"`
	Timeout  string `yaml:"timeout"`
}

// NewSubsquidUpstream builds the subsquid upstream adapter.
func NewSubsquidUpstream(cfg SubsquidProviderConfig) (*ss.Adapter, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	u := strings.TrimPrefix(endpoint, "subsquid://")
	// Validate URL shape
	if _, err := url.Parse("https://" + u); err != nil {
		return nil, err
	}

	to := 30 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil {
			to = d
		}
	}
	return ss.NewAdapter(ss.Config{
		Gateway:  u,
		ChainID:  cfg.EVM.ChainID,
		MaxChunk: cfg.MaxChunk,
		Timeout:  to,
	}), nil
}
