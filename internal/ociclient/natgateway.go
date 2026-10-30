package ociclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// NAT 网关的生命周期状态。
//
// 只有 AVAILABLE 能用来改路由：PROVISIONING 期间写进去的路由规则会被
// OCI 拒绝，TERMINATING 之后再去引用就是一个 404。
const (
	NatLifecycleProvisioning = "PROVISIONING"
	NatLifecycleAvailable    = "AVAILABLE"
	NatLifecycleUpdating     = "UPDATING"
	NatLifecycleTerminating  = "TERMINATING"
	NatLifecycleTerminated   = "TERMINATED"
)

// NatGateway 是 NAT 网关。
//
// 它让子网里的实例在没有公网 IP 的情况下也能主动出网；在这里的用途是
// 把实例的出站流量从「实例自己的公网 IP 通道」改到「NAT 通道」。
type NatGateway struct {
	ID             string            `json:"id"`
	CompartmentID  string            `json:"compartmentId"`
	VcnID          string            `json:"vcnId"`
	DisplayName    string            `json:"displayName"`
	LifecycleState string            `json:"lifecycleState"`
	BlockTraffic   bool              `json:"blockTraffic"`
	FreeformTags   map[string]string `json:"freeformTags"`
	TimeCreated    time.Time         `json:"timeCreated"`
	Region         string            `json:"-"`
}

// CreateNatGatewayRequest 是建 NAT 网关的入参。
type CreateNatGatewayRequest struct {
	CompartmentID string
	VcnID         string
	DisplayName   string
	FreeformTags  map[string]string
}

// CreateNatGateway 建一个 NAT 网关。
func (c *Client) CreateNatGateway(ctx context.Context, region string, req CreateNatGatewayRequest) (*NatGateway, error) {
	if req.CompartmentID == "" {
		req.CompartmentID = c.creds.TenancyOCID
	}
	body := map[string]any{
		"compartmentId": req.CompartmentID,
		"vcnId":         req.VcnID,
		"displayName":   req.DisplayName,
	}
	if len(req.FreeformTags) > 0 {
		body["freeformTags"] = req.FreeformTags
	}

	var out NatGateway
	_, err := c.Do(ctx, Request{
		Method: http.MethodPost, Service: ServiceCore, Path: "/natGateways",
		// 建网关不是幂等操作：重试会多建一个，而多出来的那个不会有人去删，
		// 直接变成账单上的残留。这里只靠 RetryToken 兜住「请求已到达但响应
		// 丢了」的情况，不再叠加自动重试。
		Idempotent: true,
		Region:     region,
		Body:       body,
	}, &out)
	if err != nil {
		return nil, err
	}
	out.Region = region
	return &out, nil
}

// GetNatGateway 取回 NAT 网关。
func (c *Client) GetNatGateway(ctx context.Context, region, natGatewayID string) (*NatGateway, error) {
	var out NatGateway
	_, err := c.Do(ctx, Request{
		Method: http.MethodGet, Service: ServiceCore,
		Path: "/natGateways/" + url.PathEscape(natGatewayID), Region: region,
	}, &out)
	if err != nil {
		return nil, err
	}
	out.Region = region
	return &out, nil
}

// ListNatGateways 列出 compartment（可限定 VCN）下的 NAT 网关。
//
// 已终止的实例不需要看：清理流程要判断的是「还有没有活着的」。
func (c *Client) ListNatGateways(ctx context.Context, region, compartmentID, vcnID string) ([]NatGateway, error) {
	if compartmentID == "" {
		compartmentID = c.creds.TenancyOCID
	}
	query := url.Values{}
	query.Set("compartmentId", compartmentID)
	query.Set("limit", "100")
	if vcnID != "" {
		query.Set("vcnId", vcnID)
	}

	var out []NatGateway
	_, err := c.Do(ctx, Request{
		Method: http.MethodGet, Service: ServiceCore, Path: "/natGateways",
		Region: region, Query: query,
	}, &out)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Region = region
	}
	return out, nil
}

// DeleteNatGateway 删除 NAT 网关。
//
// 已经是 TERMINATING / TERMINATED 的网关再删会返回 409，调用方应当把它当成
// 「已经删了」而不是失败——清理流程最怕的就是在最后一步因为幂等性问题卡住。
func (c *Client) DeleteNatGateway(ctx context.Context, region, natGatewayID string) error {
	_, err := c.Do(ctx, Request{
		Method: http.MethodDelete, Service: ServiceCore,
		Path: "/natGateways/" + url.PathEscape(natGatewayID), Region: region,
	}, nil)
	return err
}
