package common

// NewNormalizedResponseFromJsonRpc wraps a JsonRpcResponse into a NormalizedResponse.
func NewNormalizedResponseFromJsonRpc(jrr *JsonRpcResponse) *NormalizedResponse {
	nr := NewNormalizedResponse()
	return nr.WithJsonRpcResponse(jrr)
}
