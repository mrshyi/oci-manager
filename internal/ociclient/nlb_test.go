package ociclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NLB 走的是独立服务端点，拼错了整条 500Mbps 流程都会以 404 或超时收场，
// 而错误信息里只会说「NotAuthorizedOrNotFound」，很难指回是域名不对。
func TestNLBEndpoint(t *testing.T) {
	got := Endpoint(ServiceNetworkLoadBalancer, "ap-tokyo-1")
	want := "https://networkloadbalancer.ap-tokyo-1.oraclecloud.com"
	if got != want {
		t.Fatalf("NLB 端点 = %q，期望 %q", got, want)
	}
	if ServiceNetworkLoadBalancer.Version != "20200501" {
		t.Errorf("NLB 版本号 = %q，期望 20200501", ServiceNetworkLoadBalancer.Version)
	}
	// NLB 的域名不带 .oci：带了会解析得到但连不上，请求一直挂到超时。
	if ServiceNetworkLoadBalancer.OCIInfix {
		t.Error("NLB 端点不该带 .oci 中缀")
	}
}

// 建 NAT 网关时请求体里必须有 vcnId，且幂等键要带在头上。
func TestCreateNatGatewayRequest(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
		gotTok  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTok = r.Header.Get(retryTokenHeader)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ocid1.natgateway.oc1..x","lifecycleState":"PROVISIONING"}`))
	}))
	defer srv.Close()

	c := clientFor(t, srv)
	gw, err := c.CreateNatGateway(context.Background(), "us-ashburn-1", CreateNatGatewayRequest{
		CompartmentID: "ocid1.compartment.oc1..c",
		VcnID:         "ocid1.vcn.oc1..v",
		DisplayName:   "ocim-bw-nat-abc",
		// tag 键由 opssvc 定义（清理流程靠它反查资源），这里用字面量，
		// 避免 ociclient 反向依赖上层包。
		FreeformTags: map[string]string{"ocim-bw": "ocid1.instance.oc1..i"},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotPath != "/20160918/natGateways" {
		t.Errorf("路径 = %q，期望 /20160918/natGateways", gotPath)
	}
	if gotBody["vcnId"] != "ocid1.vcn.oc1..v" {
		t.Errorf("请求体缺 vcnId: %v", gotBody)
	}
	tags, _ := gotBody["freeformTags"].(map[string]any)
	if tags["ocim-bw"] != "ocid1.instance.oc1..i" {
		t.Errorf("请求体缺清理用的 tag: %v", gotBody)
	}
	if gotTok == "" {
		t.Error("建 NAT 网关必须带幂等键")
	}
	if gw.ID == "" || gw.LifecycleState != NatLifecycleProvisioning {
		t.Errorf("返回解析不对: %+v", gw)
	}
}

// 建 NLB 的请求必须走 networkloadbalancer 服务，且 isPrivate 显式为 false
// （公网 NLB 才有公网 IP，也才有带宽上的意义）。
func TestCreateNetworkLoadBalancerRequest(t *testing.T) {
	var (
		gotHost string
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ocid1.networkloadbalancer.oc1..x","lifecycleState":"CREATING",
			"ipAddresses":[{"ipAddress":"10.0.0.9","isPublic":false},{"ipAddress":"198.51.100.7","isPublic":true}]}`))
	}))
	defer srv.Close()

	c := clientFor(t, srv)
	nlb, err := c.CreateNetworkLoadBalancer(context.Background(), "ap-tokyo-1", CreateNLBRequest{
		CompartmentID: "ocid1.compartment.oc1..c",
		SubnetID:      "ocid1.subnet.oc1..s",
		DisplayName:   "ocim-bw-nlb-abc",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// rewriteTransport 会改 Host 头，所以这里只能断言路径；域名由
	// TestNLBEndpoint 单独钉住。
	if gotPath != "/20200501/networkLoadBalancers" {
		t.Errorf("路径 = %q，期望 /20200501/networkLoadBalancers", gotPath)
	}
	if gotHost == "" {
		t.Error("没有收到 Host 头")
	}
	if gotBody["isPrivate"] != false {
		t.Errorf("isPrivate 必须显式为 false，实际 %v", gotBody["isPrivate"])
	}
	if gotBody["subnetId"] != "ocid1.subnet.oc1..s" {
		t.Errorf("请求体缺 subnetId: %v", gotBody)
	}
	if len(nlb.IpAddresses) != 2 {
		t.Fatalf("IP 列表解析不对: %+v", nlb.IpAddresses)
	}
}

// 删除 NLB 走 DELETE，且路径要带 NLB 的 OCID。
func TestDeleteNetworkLoadBalancerPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := clientFor(t, srv)
	if err := c.DeleteNetworkLoadBalancer(context.Background(), "ap-tokyo-1", "ocid1.networkloadbalancer.oc1..abc"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("方法 = %s，期望 DELETE", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/networkLoadBalancers/ocid1.networkloadbalancer.oc1..abc") {
		t.Errorf("路径 = %q，期望以 NLB OCID 结尾", gotPath)
	}
}

// 404 / 409 的判定是清理流程幂等性的基础，判错会导致「已经删掉了」被当成
// 失败，或者「别人家的资源」被当成自己的。
func TestIsNotFoundAndConflict(t *testing.T) {
	notFound := &APIError{StatusCode: http.StatusNotFound, Code: "NotAuthorizedOrNotFound"}
	if !IsNotFound(notFound) {
		t.Error("404 应当被 IsNotFound 认出")
	}
	if IsNotFound(&APIError{StatusCode: http.StatusConflict}) {
		t.Error("409 不该被当成 404")
	}
	conflict := &APIError{StatusCode: http.StatusConflict, Code: "Conflict"}
	if !IsConflict(conflict) {
		t.Error("409 应当被 IsConflict 认出")
	}
	if IsNotFound(nil) || IsConflict(nil) {
		t.Error("nil 不该被判成任何一类")
	}
}
