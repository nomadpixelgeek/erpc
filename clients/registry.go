package clients

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type ClientType string

const (
	ClientTypeHttpJsonRpc ClientType = "HttpJsonRpc"
	ClientTypeGrpcBds     ClientType = "GrpcBds"
)

// --- Adapter to wrap an injected common.UpstreamClient into a ClientInterface ---

type vendorClientAdapter struct {
	inner common.UpstreamClient
}

// We deliberately return HttpJsonRpc here so Upstream.Forward() goes through the
// existing code path that handles ClientTypeHttpJsonRpc / ClientTypeGrpcBds.
// The actual transport is implemented by the vendor client itself.
func (a *vendorClientAdapter) GetType() ClientType {
	return ClientTypeHttpJsonRpc
}

func (a *vendorClientAdapter) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := a.inner.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponseFromJsonRpc(jrr), nil
}

type ClientInterface interface {
	GetType() ClientType
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

type Client struct {
	Upstream common.Upstream
}

type ClientRegistry struct {
	logger            *zerolog.Logger
	projectId         string
	clients           sync.Map
	proxyPoolRegistry *ProxyPoolRegistry
	evmExtractor      common.JsonRpcErrorExtractor
}

func NewClientRegistry(logger *zerolog.Logger, projectId string, proxyPoolRegistry *ProxyPoolRegistry, evmExtractor common.JsonRpcErrorExtractor) *ClientRegistry {
	cr := &ClientRegistry{
		logger:            logger,
		projectId:         projectId,
		proxyPoolRegistry: proxyPoolRegistry,
		evmExtractor:      evmExtractor,
	}
	return cr
}

func (manager *ClientRegistry) GetOrCreateClient(appCtx context.Context, ups common.Upstream) (ClientInterface, error) {
	if client, ok := manager.clients.Load(common.UniqueUpstreamKey(ups)); ok {
		return client.(ClientInterface), nil
	}
	return manager.CreateClient(appCtx, ups)
}

func (manager *ClientRegistry) CreateClient(appCtx context.Context, ups common.Upstream) (ClientInterface, error) {
	var once sync.Once
	var newClient ClientInterface
	var clientErr error

	cfg := ups.Config()

	// 🔹 First, check if the upstream already has an injected vendor client.
	// We look for a narrow interface to avoid importing upstream package here.
	type upstreamWithInjectedClient interface {
		InjectedClient() common.UpstreamClient
	}
	if uwi, ok := ups.(upstreamWithInjectedClient); ok {
		if injected := uwi.InjectedClient(); injected != nil {
			adapted := &vendorClientAdapter{inner: injected}
			manager.clients.Store(common.UniqueUpstreamKey(ups), adapted)
			return adapted, nil
		}
	}

	// 🔸 Fallback: build a client based on endpoint (existing behavior).
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("upstream endpoint is required")
	}

	parsedUrl, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL for upstream: %v", cfg.Id)
	}

	var proxyPool *ProxyPool
	if cfg.JsonRpc != nil && cfg.JsonRpc.ProxyPool != "" {
		proxyPool, err = manager.proxyPoolRegistry.GetPool(cfg.JsonRpc.ProxyPool)
		if err != nil {
			return nil, fmt.Errorf("failed to get proxy pool: %v", cfg.Id)
		}
	}

	if err != nil {
		clientErr = fmt.Errorf("failed to parse URL for upstream: %v", cfg.Id)
	} else {
		once.Do(func() {
			lg := manager.logger.With().Str("upstreamId", cfg.Id).Logger()
			switch cfg.Type {
			case common.UpstreamTypeEvm:
				if parsedUrl.Scheme == "http" || parsedUrl.Scheme == "https" {
					newClient, err = NewGenericHttpJsonRpcClient(
						appCtx,
						&lg,
						manager.projectId,
						ups,
						parsedUrl,
						cfg.JsonRpc,
						proxyPool,
						manager.evmExtractor,
					)
					if err != nil {
						clientErr = fmt.Errorf("failed to create HTTP client for upstream: %v", cfg.Id)
					}
				} else if parsedUrl.Scheme == "ws" || parsedUrl.Scheme == "wss" {
					clientErr = fmt.Errorf("websocket client not implemented yet")
				} else if parsedUrl.Scheme == "grpc" || parsedUrl.Scheme == "grpc+bds" {
					newClient, err = NewGrpcBdsClient(
						appCtx,
						&lg,
						manager.projectId,
						ups,
						parsedUrl,
					)
					if err != nil {
						clientErr = fmt.Errorf("failed to create gRPC BDS client for upstream: %v", cfg.Id)
					}
				} else {
					clientErr = fmt.Errorf("unsupported endpoint scheme: %v for upstream: %v", parsedUrl.Scheme, cfg.Id)
				}
			default:
				clientErr = fmt.Errorf("unsupported upstream type: %v for upstream: %v", cfg.Type, cfg.Id)
			}

			if clientErr == nil {
				manager.clients.Store(common.UniqueUpstreamKey(ups), newClient)
			}
		})
	}

	return newClient, clientErr
}
