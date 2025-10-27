package providers

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	ss "erpc/upstream/evm/subsquid"
)

// Provider factory wiring. Adjust to erpc project's provider registry.

type SubsquidProviderConfig struct {
	Endpoint string `yaml:"endpoint"` // subsquid://v2.archive.subsquid.io/network/base-mainnet
	EVM struct {
		ChainID uint64 `yaml:"chainId"`
	} `yaml:"evm"`
	JSONRPC struct {
		SupportsBatch bool `yaml:"supportsBatch"`
	} `yaml:"jsonRpc"`
	Failsafe any `yaml:"failsafe"`
	// Optional
	MaxChunk uint64 `yaml:"maxChunk"`
	Timeout  string `yaml:"timeout"`
}

func NewSubsquidUpstream(cfg SubsquidProviderConfig) (*ss.Adapter, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if !strings.HasPrefix(endpoint, "subsquid://") {
		return nil, fmt.Errorf("subsquid endpoint must start with subsquid://")
	}
	u := strings.TrimPrefix(endpoint, "subsquid://")
	if _, err := url.Parse("https://" + u); err != nil {
		return nil, fmt.Errorf("invalid subsquid url: %w", err)
	}

	to := 30 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil {
			to = d
		}
	}

	return ss.NewAdapter(ss.Config{
		Gateway: u,
		ChainID: cfg.EVM.ChainID,
		MaxChunk: cfg.MaxChunk,
		Timeout:  to,
	}), nil
}
