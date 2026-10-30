package ociclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// NLB 的生命周期状态。
//
// 创建与删除都是异步的：接口立刻返回 202，真正的状态变化要靠轮询这里
// 的取值来确认。只有 ACTIVE 才能加后端集，DELETED 才算真正删干净。
const (
	NLBLifecycleCreating  = "CREATING"
	NLBLifecycleActive    = "ACTIVE"
	NLBLifecycleUpdating  = "UPDATING"
	NLBLifecycleDeleting  = "DELETING"
	NLBLifecycleDeleted   = "DELETED"
	NLBLifecycleFailed    = "FAILED"
	NLBLifecycleSuspended = "SUSPENDED"
)

// NLB 的协议取值。
const (
	NLBProtocolTCP = "TCP"
	NLBProtocolUDP = "UDP"
)

// NLB 后端集的策略。
//
// NLB 的可用策略比传统负载均衡少：五元组哈希是默认值，两个后端时用它
// 能保证同一条连接固定落到同一台机器上。
const (
	NLBPolicyFiveTuple  = "FIVE_TUPLE"
	NLBPolicyThreeTuple = "THREE_TUPLE"
	NLBPolicyTwoTuple   = "TWO_TUPLE"
	NLBPolicyRoundRobin = "ROUND_ROBIN"
	NLBPolicyLeastConn  = "LEAST_CONNECTIONS"
	NLBPolicyIPHash     = "IP_HASH"
)

// NLBIpAddress 是 NLB 上的一个 IP。
type NLBIpAddress struct {
	IPAddress string `json:"ipAddress"`
	// IsPublic 为 true 表示这个地址对公网可达。公网 NLB 才会有。
	IsPublic bool `json:"isPublic"`
}

// NetworkLoadBalancer 是网络负载均衡（四层）。
//
// 与传统负载均衡的区别：它工作在 TCP/UDP 层，不解析 HTTP，所以带宽和连接数
// 的开销小得多——这也是「500Mbps」这件事借用它的原因。
type NetworkLoadBalancer struct {
	ID             string            `json:"id"`
	CompartmentID  string            `json:"compartmentId"`
	DisplayName    string            `json:"displayName"`
	SubnetID       string            `json:"subnetId"`
	IsPrivate      bool              `json:"isPrivate"`
	LifecycleState string            `json:"lifecycleState"`
	IpAddresses    []NLBIpAddress    `json:"ipAddresses"`
	FreeformTags   map[string]string `json:"freeformTags"`
	TimeCreated    time.Time         `json:"timeCreated"`
	Region         string            `json:"-"`
}

// NLBHealthChecker 是后端健康检查配置。
type NLBHealthChecker struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	// IntervalInMillis / TimeoutInMillis 的合法区间是 1000–30000 / 1000–10000。
	IntervalInMillis int `json:"intervalInMillis,omitempty"`
	TimeoutInMillis  int `json:"timeoutInMillis,omitempty"`
	Retries          int `json:"retries,omitempty"`
}

// CreateNLBRequest 是建 NLB 的入参。
type CreateNLBRequest struct {
	CompartmentID string
	SubnetID      string
	DisplayName   string
	// IsPrivate 为 false 时 OCI 会自动分配一个公网 IP。
	IsPrivate    bool
	FreeformTags map[string]string
}

// CreateBackendSetRequest 是建后端集的入参。
type CreateBackendSetRequest struct {
	Name          string
	Policy        string
	HealthChecker NLBHealthChecker
}

// CreateBackendRequest 是往后端集里加一台机器的入参。
type CreateBackendRequest struct {
	// IPAddress 是实例的私网 IP，不是公网 IP：NLB 与后端在同一个 VCN 内通信。
	IPAddress string
	Port      int
	Weight    int
}

// CreateListenerRequest 是建监听的入参。
//
// NLB 的监听是「一个端口一个监听」，没有「监听全部端口」这种写法。
// 想要多个端口就得建多个监听，每个监听再指回同一个后端集。
type CreateListenerRequest struct {
	Name                  string
	DefaultBackendSetName string
	Port                  int
	Protocol              string
}

// CreateNetworkLoadBalancer 建一个 NLB。
//
// 返回的对象里已经带着 ID，但生命周期状态是 CREATING——真正的可用要等
// GetNetworkLoadBalancer 看到 ACTIVE。
func (c *Client) CreateNetworkLoadBalancer(ctx context.Context, region string, req CreateNLBRequest) (*NetworkLoadBalancer, error) {
	if req.CompartmentID == "" {
		req.CompartmentID = c.creds.TenancyOCID
	}
	body := map[string]any{
		"compartmentId": req.CompartmentID,
		"subnetId":      req.SubnetID,
		"displayName":   req.DisplayName,
		"isPrivate":     req.IsPrivate,
	}
	if len(req.FreeformTags) > 0 {
		body["freeformTags"] = req.FreeformTags
	}

	var out NetworkLoadBalancer
	_, err := c.Do(ctx, Request{
		Method: http.MethodPost, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers", Region: region, Body: body,
		// 建 NLB 会直接产生计费资源，重试必须靠幂等键去重，
		// 否则一次超时重试就会多一个没人认领的 NLB。
		Idempotent: true,
	}, &out)
	if err != nil {
		return nil, err
	}
	out.Region = region
	return &out, nil
}

// GetNetworkLoadBalancer 取回 NLB。
func (c *Client) GetNetworkLoadBalancer(ctx context.Context, region, nlbID string) (*NetworkLoadBalancer, error) {
	var out NetworkLoadBalancer
	_, err := c.Do(ctx, Request{
		Method: http.MethodGet, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers/" + url.PathEscape(nlbID), Region: region,
	}, &out)
	if err != nil {
		return nil, err
	}
	out.Region = region
	return &out, nil
}

// ListNetworkLoadBalancers 列出 compartment 下的 NLB。
func (c *Client) ListNetworkLoadBalancers(ctx context.Context, region, compartmentID string) ([]NetworkLoadBalancer, error) {
	if compartmentID == "" {
		compartmentID = c.creds.TenancyOCID
	}
	query := url.Values{}
	query.Set("compartmentId", compartmentID)
	query.Set("limit", "100")

	var out []NetworkLoadBalancer
	_, err := c.Do(ctx, Request{
		Method: http.MethodGet, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers", Region: region, Query: query,
	}, &out)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Region = region
	}
	return out, nil
}

// DeleteNetworkLoadBalancer 删除 NLB。
//
// 异步：返回 202 之后状态会先变 DELETING，等 DELETED 才算删干净。删除 NLB
// 会连带删掉它的后端集与监听，所以清理流程不需要逐个先删。
func (c *Client) DeleteNetworkLoadBalancer(ctx context.Context, region, nlbID string) error {
	_, err := c.Do(ctx, Request{
		Method: http.MethodDelete, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers/" + url.PathEscape(nlbID), Region: region,
	}, nil)
	return err
}

// CreateNLBBackendSet 建后端集。
func (c *Client) CreateNLBBackendSet(ctx context.Context, region, nlbID string, req CreateBackendSetRequest) error {
	hc := req.HealthChecker
	if hc.Protocol == "" {
		hc.Protocol = NLBProtocolTCP
	}
	policy := req.Policy
	if policy == "" {
		policy = NLBPolicyFiveTuple
	}
	_, err := c.Do(ctx, Request{
		Method: http.MethodPost, Service: ServiceNetworkLoadBalancer,
		Path:       "/networkLoadBalancers/" + url.PathEscape(nlbID) + "/backendSets",
		Region:     region,
		Idempotent: true,
		Body: map[string]any{
			"name":          req.Name,
			"policy":        policy,
			"healthChecker": hc,
		},
	}, nil)
	return err
}

// DeleteNLBBackendSet 删后端集。
func (c *Client) DeleteNLBBackendSet(ctx context.Context, region, nlbID, name string) error {
	_, err := c.Do(ctx, Request{
		Method: http.MethodDelete, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers/" + url.PathEscape(nlbID) +
			"/backendSets/" + url.PathEscape(name),
		Region: region,
	}, nil)
	return err
}

// CreateNLBBackend 往后端集里加一台机器。
func (c *Client) CreateNLBBackend(ctx context.Context, region, nlbID, backendSetName string, req CreateBackendRequest) error {
	weight := req.Weight
	if weight == 0 {
		weight = 1
	}
	_, err := c.Do(ctx, Request{
		Method: http.MethodPost, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers/" + url.PathEscape(nlbID) +
			"/backendSets/" + url.PathEscape(backendSetName) + "/backends",
		Region:     region,
		Idempotent: true,
		Body: map[string]any{
			"ipAddress": req.IPAddress,
			"port":      req.Port,
			"weight":    weight,
		},
	}, nil)
	return err
}

// CreateNLBListener 建监听。
func (c *Client) CreateNLBListener(ctx context.Context, region, nlbID string, req CreateListenerRequest) error {
	protocol := req.Protocol
	if protocol == "" {
		protocol = NLBProtocolTCP
	}
	_, err := c.Do(ctx, Request{
		Method: http.MethodPost, Service: ServiceNetworkLoadBalancer,
		Path:       "/networkLoadBalancers/" + url.PathEscape(nlbID) + "/listeners",
		Region:     region,
		Idempotent: true,
		Body: map[string]any{
			"name":                  req.Name,
			"defaultBackendSetName": req.DefaultBackendSetName,
			"port":                  req.Port,
			"protocol":              protocol,
		},
	}, nil)
	return err
}

// DeleteNLBListener 删监听。
func (c *Client) DeleteNLBListener(ctx context.Context, region, nlbID, name string) error {
	_, err := c.Do(ctx, Request{
		Method: http.MethodDelete, Service: ServiceNetworkLoadBalancer,
		Path: "/networkLoadBalancers/" + url.PathEscape(nlbID) +
			"/listeners/" + url.PathEscape(name),
		Region: region,
	}, nil)
	return err
}
