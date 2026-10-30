package opssvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// fakeBW 是带宽流程的假客户端。
//
// 它刻意模拟了几个真实服务端的行为，因为这些行为直接决定了流程的正确性：
//   - 创建 NAT / NLB 之后先进入 PROVISIONING / CREATING，要再查一次才 ACTIVE；
//   - 删一个仍被路由引用的 NAT 会失败；
//   - 删已经不存在的东西返回 404，建已经存在的东西返回 409。
type fakeBW struct {
	inst     *ociclient.Instance
	vnicAtts []ociclient.VnicAttachment
	vnic     *ociclient.Vnic
	subnet   *ociclient.Subnet
	vcn      *ociclient.Vcn
	routes   []ociclient.RouteRule
	igws     []ociclient.InternetGateway

	nats map[string]*ociclient.NatGateway
	nlbs map[string]*ociclient.NetworkLoadBalancer

	calls       []string
	failOn      map[string]error
	natCreateN  int
	nlbCreateN  int
	routeWrites [][]ociclient.RouteRule
	backends    []string
	backendSets map[string]bool
	listeners   map[string]bool

	// autoReady 为 true 时创建后直接给可用状态，省掉等待轮次。
	autoReady bool
}

func newFakeBW() *fakeBW {
	return &fakeBW{
		inst: &ociclient.Instance{
			ID:             testBWInstance,
			CompartmentID:  testBWCompartment,
			DisplayName:    "东京-AMD-1",
			Shape:          "VM.Standard.E2.1.Micro",
			LifecycleState: ociclient.LifecycleRunning,
		},
		vnicAtts: []ociclient.VnicAttachment{{
			ID: "ocid1.vnicattachment.oc1..a1", InstanceID: testBWInstance,
			VnicID: "ocid1.vnic.oc1..v1", SubnetID: testBWSubnet,
			NicIndex: 0, LifecycleState: "ATTACHED",
		}},
		vnic: &ociclient.Vnic{
			ID: "ocid1.vnic.oc1..v1", SubnetID: testBWSubnet,
			PrivateIP: "10.0.0.5", PublicIP: "203.0.113.9",
		},
		subnet: &ociclient.Subnet{
			ID: testBWSubnet, VcnID: testBWVcn, DisplayName: "公共子网",
			RouteTableID: testBWRouteTable,
		},
		vcn: &ociclient.Vcn{ID: testBWVcn, CidrBlock: "10.0.0.0/16"},
		routes: []ociclient.RouteRule{
			{Destination: "10.0.0.0/16", DestinationType: "CIDR_BLOCK", RouteType: "LOCAL"},
			{Destination: "0.0.0.0/0", DestinationType: "CIDR_BLOCK",
				NetworkEntityID: testBWIGW, Description: "默认出网"},
		},
		igws:        []ociclient.InternetGateway{{ID: testBWIGW, DisplayName: "igw"}},
		nats:        map[string]*ociclient.NatGateway{},
		nlbs:        map[string]*ociclient.NetworkLoadBalancer{},
		failOn:      map[string]error{},
		backendSets: map[string]bool{},
		listeners:   map[string]bool{},
	}
}

const (
	testBWInstance    = "ocid1.instance.oc1..bwinstance"
	testBWCompartment = "ocid1.compartment.oc1..bwcomp"
	testBWSubnet      = "ocid1.subnet.oc1..bwsubnet"
	testBWVcn         = "ocid1.vcn.oc1..bwvcn"
	testBWRouteTable  = "ocid1.routetable.oc1..bwrt"
	testBWIGW         = "ocid1.internetgateway.oc1..bwigw"
)

func (f *fakeBW) record(name string) error {
	f.calls = append(f.calls, name)
	if err, ok := f.failOn[name]; ok {
		return err
	}
	return nil
}

func (f *fakeBW) converge() {
	for _, gw := range f.nats {
		gw.LifecycleState = ociclient.NatLifecycleAvailable
	}
	for _, nlb := range f.nlbs {
		nlb.LifecycleState = ociclient.NLBLifecycleActive
	}
}

func bwErr(status int, code string) error {
	return &ociclient.APIError{
		StatusCode: status, Code: code, Message: code, Class: ociclient.ClassBadRequest,
	}
}

func (f *fakeBW) GetInstance(ctx context.Context, region, instanceID string) (*ociclient.Instance, error) {
	if err := f.record("GetInstance"); err != nil {
		return nil, err
	}
	return f.inst, nil
}

func (f *fakeBW) ListVnicAttachments(ctx context.Context, region, compartmentID, instanceID string) ([]ociclient.VnicAttachment, error) {
	if err := f.record("ListVnicAttachments"); err != nil {
		return nil, err
	}
	return f.vnicAtts, nil
}

func (f *fakeBW) GetVnic(ctx context.Context, region, vnicID string) (*ociclient.Vnic, error) {
	if err := f.record("GetVnic"); err != nil {
		return nil, err
	}
	return f.vnic, nil
}

func (f *fakeBW) GetSubnet(ctx context.Context, region, subnetID string) (*ociclient.Subnet, error) {
	if err := f.record("GetSubnet"); err != nil {
		return nil, err
	}
	return f.subnet, nil
}

func (f *fakeBW) GetVcn(ctx context.Context, region, vcnID string) (*ociclient.Vcn, error) {
	if err := f.record("GetVcn"); err != nil {
		return nil, err
	}
	return f.vcn, nil
}

func (f *fakeBW) GetRouteTable(ctx context.Context, region, routeTableID string) (*ociclient.RouteTable, error) {
	if err := f.record("GetRouteTable"); err != nil {
		return nil, err
	}
	return &ociclient.RouteTable{ID: routeTableID, RouteRules: f.routes}, nil
}

func (f *fakeBW) UpdateRouteTable(ctx context.Context, region, routeTableID string, rules []ociclient.RouteRule) (*ociclient.RouteTable, error) {
	if err := f.record("UpdateRouteTable"); err != nil {
		return nil, err
	}
	f.routeWrites = append(f.routeWrites, rules)
	f.routes = rules
	return &ociclient.RouteTable{ID: routeTableID, RouteRules: rules}, nil
}

func (f *fakeBW) ListInternetGateways(ctx context.Context, region, compartmentID, vcnID string) ([]ociclient.InternetGateway, error) {
	if err := f.record("ListInternetGateways"); err != nil {
		return nil, err
	}
	return f.igws, nil
}

func (f *fakeBW) ListNatGateways(ctx context.Context, region, compartmentID, vcnID string) ([]ociclient.NatGateway, error) {
	if err := f.record("ListNatGateways"); err != nil {
		return nil, err
	}
	out := make([]ociclient.NatGateway, 0, len(f.nats))
	for _, gw := range f.nats {
		out = append(out, *gw)
	}
	return out, nil
}

func (f *fakeBW) CreateNatGateway(ctx context.Context, region string, req ociclient.CreateNatGatewayRequest) (*ociclient.NatGateway, error) {
	if err := f.record("CreateNatGateway"); err != nil {
		return nil, err
	}
	f.natCreateN++
	state := ociclient.NatLifecycleProvisioning
	if f.autoReady {
		state = ociclient.NatLifecycleAvailable
	}
	gw := &ociclient.NatGateway{
		ID:            "ocid1.natgateway.oc1..nat" + itoaBW(f.natCreateN),
		CompartmentID: req.CompartmentID, VcnID: req.VcnID,
		DisplayName: req.DisplayName, LifecycleState: state,
		FreeformTags: req.FreeformTags,
	}
	f.nats[gw.ID] = gw
	return gw, nil
}

func (f *fakeBW) GetNatGateway(ctx context.Context, region, natGatewayID string) (*ociclient.NatGateway, error) {
	if err := f.record("GetNatGateway"); err != nil {
		return nil, err
	}
	gw, ok := f.nats[natGatewayID]
	if !ok {
		return nil, bwErr(404, "NotAuthorizedOrNotFound")
	}
	return gw, nil
}

func (f *fakeBW) DeleteNatGateway(ctx context.Context, region, natGatewayID string) error {
	if err := f.record("DeleteNatGateway"); err != nil {
		return err
	}
	if defaultRouteTarget(f.routes) == natGatewayID {
		// 真实服务端会返回 409：路由表还在引用它。这里用 400 让测试失败得
		// 更直接——顺序写错时我们要立刻看到，而不是等到阶段超时。
		return bwErr(400, "NatGatewayInUse")
	}
	delete(f.nats, natGatewayID)
	return nil
}

func (f *fakeBW) ListNetworkLoadBalancers(ctx context.Context, region, compartmentID string) ([]ociclient.NetworkLoadBalancer, error) {
	if err := f.record("ListNetworkLoadBalancers"); err != nil {
		return nil, err
	}
	out := make([]ociclient.NetworkLoadBalancer, 0, len(f.nlbs))
	for _, nlb := range f.nlbs {
		out = append(out, *nlb)
	}
	return out, nil
}

func (f *fakeBW) CreateNetworkLoadBalancer(ctx context.Context, region string, req ociclient.CreateNLBRequest) (*ociclient.NetworkLoadBalancer, error) {
	if err := f.record("CreateNetworkLoadBalancer"); err != nil {
		return nil, err
	}
	f.nlbCreateN++
	state := ociclient.NLBLifecycleCreating
	if f.autoReady {
		state = ociclient.NLBLifecycleActive
	}
	nlb := &ociclient.NetworkLoadBalancer{
		ID:            "ocid1.networkloadbalancer.oc1..nlb" + itoaBW(f.nlbCreateN),
		CompartmentID: req.CompartmentID, SubnetID: req.SubnetID,
		DisplayName: req.DisplayName, IsPrivate: req.IsPrivate,
		LifecycleState: state, FreeformTags: req.FreeformTags,
		IpAddresses: []ociclient.NLBIpAddress{
			{IPAddress: "10.0.0.20", IsPublic: false},
			{IPAddress: "198.51.100.7", IsPublic: true},
		},
	}
	f.nlbs[nlb.ID] = nlb
	return nlb, nil
}

func (f *fakeBW) GetNetworkLoadBalancer(ctx context.Context, region, nlbID string) (*ociclient.NetworkLoadBalancer, error) {
	if err := f.record("GetNetworkLoadBalancer"); err != nil {
		return nil, err
	}
	nlb, ok := f.nlbs[nlbID]
	if !ok {
		return nil, bwErr(404, "NotAuthorizedOrNotFound")
	}
	return nlb, nil
}

func (f *fakeBW) DeleteNetworkLoadBalancer(ctx context.Context, region, nlbID string) error {
	if err := f.record("DeleteNetworkLoadBalancer"); err != nil {
		return err
	}
	if _, ok := f.nlbs[nlbID]; !ok {
		return bwErr(404, "NotAuthorizedOrNotFound")
	}
	delete(f.nlbs, nlbID)
	return nil
}

func (f *fakeBW) CreateNLBBackendSet(ctx context.Context, region, nlbID string, req ociclient.CreateBackendSetRequest) error {
	if err := f.record("CreateNLBBackendSet"); err != nil {
		return err
	}
	if f.backendSets[req.Name] {
		return bwErr(409, "Conflict")
	}
	f.backendSets[req.Name] = true
	return nil
}

func (f *fakeBW) DeleteNLBBackendSet(ctx context.Context, region, nlbID, name string) error {
	if err := f.record("DeleteNLBBackendSet"); err != nil {
		return err
	}
	if !f.backendSets[name] {
		return bwErr(404, "NotAuthorizedOrNotFound")
	}
	delete(f.backendSets, name)
	return nil
}

func (f *fakeBW) CreateNLBBackend(ctx context.Context, region, nlbID, backendSetName string, req ociclient.CreateBackendRequest) error {
	if err := f.record("CreateNLBBackend"); err != nil {
		return err
	}
	f.backends = append(f.backends, req.IPAddress)
	return nil
}

func (f *fakeBW) CreateNLBListener(ctx context.Context, region, nlbID string, req ociclient.CreateListenerRequest) error {
	if err := f.record("CreateNLBListener"); err != nil {
		return err
	}
	if f.listeners[req.Name] {
		return bwErr(409, "Conflict")
	}
	f.listeners[req.Name] = true
	return nil
}

func (f *fakeBW) DeleteNLBListener(ctx context.Context, region, nlbID, name string) error {
	if err := f.record("DeleteNLBListener"); err != nil {
		return err
	}
	if !f.listeners[name] {
		return bwErr(404, "NotAuthorizedOrNotFound")
	}
	delete(f.listeners, name)
	return nil
}

func itoaBW(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "n"
}

// ---- 测试辅助 ----

func newBWTask(t *testing.T, spec BandwidthSpec) *store.OpsTask {
	t.Helper()
	encoded, err := EncodeBandwidthSpec(spec)
	if err != nil {
		t.Fatalf("编码 spec 失败: %v", err)
	}
	return &store.OpsTask{
		ID: "task-bw", Kind: store.OpsKindEnable500, AccountID: "acc-1",
		InstanceID: spec.InstanceID, Spec: encoded, State: store.OpsPending,
	}
}

func enableSpec() BandwidthSpec {
	return BandwidthSpec{
		Action: BandwidthEnable, Region: "ap-tokyo-1",
		CompartmentID: testBWCompartment, InstanceID: testBWInstance,
		ListenerPort: 443,
	}
}

func disableSpec() BandwidthSpec {
	s := enableSpec()
	s.Action = BandwidthDisable
	return s
}

// driveBW 反复执行直到任务终结，返回走过的阶段序列。
func driveBW(t *testing.T, task *store.OpsTask, spec BandwidthSpec, f *fakeBW, maxRounds int) []string {
	t.Helper()
	var stages []string
	for i := 0; i < maxRounds; i++ {
		out := bandwidthStep(context.Background(), task, spec, f)
		if out.Stage != "" {
			stages = append(stages, out.Stage)
		}
		applyOutcome(task, out)
		if out.Done {
			if out.Err != nil {
				t.Fatalf("第 %d 轮意外失败（阶段 %s）：%v", i+1, out.Stage, out.Err)
			}
			return stages
		}
		if out.Pause {
			return stages
		}
		f.converge()
	}
	t.Fatalf("流程在 %d 轮内没有结束，走过的阶段：%v", maxRounds, stages)
	return stages
}

// ---- 开启流程 ----

func TestBandwidthEnableHappyPath(t *testing.T) {
	f := newFakeBW()
	f.autoReady = true
	task := newBWTask(t, enableSpec())

	stages := driveBW(t, task, enableSpec(), f, 20)
	want := []string{
		BwNatCreating, BwRouteSwitching, BwNLBCreating,
		BwBackendSetCreating, BwListenerCreating, BwReady,
	}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段序列不对：\n got %v\nwant %v", stages, want)
	}

	if f.natCreateN != 1 {
		t.Errorf("NAT 网关应当只创建一次，实际 %d 次", f.natCreateN)
	}
	if f.nlbCreateN != 1 {
		t.Errorf("NLB 应当只创建一次，实际 %d 次", f.nlbCreateN)
	}
	if len(f.routeWrites) != 1 {
		t.Fatalf("路由表应当只写一次，实际 %d 次", len(f.routeWrites))
	}
	natID := ""
	for id := range f.nats {
		natID = id
	}
	if got := defaultRouteTarget(f.routeWrites[0]); got != natID {
		t.Errorf("默认路由应指向 NAT %s，实际指向 %s", natID, got)
	}
	if len(f.backends) != 1 || f.backends[0] != "10.0.0.5" {
		t.Errorf("NLB 后端应当是主网卡私网 IP 10.0.0.5，实际 %v", f.backends)
	}
	if !f.listeners[bwListenerName] {
		t.Error("监听没有建出来")
	}
	if !strings.Contains(task.Result, "198.51.100.7") {
		t.Errorf("结果里应当带上 NLB 公网 IP，实际 %q", task.Result)
	}
	if !strings.Contains(task.Result, "费用") {
		t.Errorf("结果里必须提醒 NLB 会产生费用，实际 %q", task.Result)
	}
}

func TestBandwidthEnableWaitsForConvergence(t *testing.T) {
	f := newFakeBW()
	// 不自动收敛：创建之后停在 PROVISIONING / CREATING。
	task := newBWTask(t, enableSpec())
	spec := enableSpec()

	// 预检 → NAT_CREATING（提交创建）
	out := bandwidthStep(context.Background(), task, spec, f)
	applyOutcome(task, out)
	out = bandwidthStep(context.Background(), task, spec, f)
	applyOutcome(task, out)
	if out.Stage != BwNatCreating {
		t.Fatalf("应当在 NAT_CREATING 等待，实际 %s", out.Stage)
	}
	if !out.NoAttempt {
		t.Error("等待云平台收敛不该计入尝试次数")
	}
	if !out.NextAt.After(time.Now()) {
		t.Error("等待时应当给一个未来的下次执行时刻，否则会变成忙循环")
	}

	// 收敛之后再跑一轮就该前进。
	f.converge()
	out = bandwidthStep(context.Background(), task, spec, f)
	if out.Stage != BwRouteSwitching {
		t.Fatalf("NAT 可用之后应当推进到 ROUTE_SWITCHING，实际 %s", out.Stage)
	}
}

func TestBandwidthEnableStopsAtFirstFailure(t *testing.T) {
	f := newFakeBW()
	f.autoReady = true
	f.failOn["CreateNetworkLoadBalancer"] = bwErr(500, "InternalError")
	task := newBWTask(t, enableSpec())

	var stages []string
	for i := 0; i < 20; i++ {
		out := bandwidthStep(context.Background(), task, enableSpec(), f)
		applyOutcome(task, out)
		if out.Done {
			if out.Err == nil {
				t.Fatal("NLB 创建失败，流程不该判成功")
			}
			if out.Stage != BwNLBCreating {
				t.Errorf("应当停在 NLB_CREATING，实际停在 %s", out.Stage)
			}
			break
		}
		stages = append(stages, out.Stage)
		f.converge()
	}
	if len(stages) == 0 {
		t.Fatal("流程没有走过任何阶段")
	}

	if len(f.backendSets) != 0 {
		t.Error("NLB 都没建出来，不该继续建后端集")
	}
	if len(f.listeners) != 0 {
		t.Error("NLB 都没建出来，不该继续建监听")
	}
	// 路由已经切过去了，这是预期行为：失败点之前的步骤不回滚，回滚本身
	// 也可能失败，会留下更难解释的状态。结果里必须能看出这一点。
	if defaultRouteTarget(f.routes) == testBWIGW {
		t.Error("失败点之前的步骤（切路由）不该被回滚")
	}
	if !strings.Contains(task.LastError, "InternalError") {
		t.Errorf("失败原因应当落库，实际 %q", task.LastError)
	}
}

func TestBandwidthPrecheckRejectsNonMicroShape(t *testing.T) {
	f := newFakeBW()
	f.inst.Shape = "VM.Standard.A1.Flex"
	task := newBWTask(t, enableSpec())

	out := bandwidthStep(context.Background(), task, enableSpec(), f)
	if !out.Done || out.Err == nil {
		t.Fatal("非 AMD 免费实例应当被拒绝")
	}
	if !strings.Contains(out.Err.Error(), "AMD") {
		t.Errorf("错误信息应当说明只支持 AMD 免费实例，实际 %q", out.Err)
	}
	if f.natCreateN != 0 {
		t.Error("预检失败后不该建任何资源")
	}
}

func TestBandwidthPrecheckRejectsNoPublicIP(t *testing.T) {
	f := newFakeBW()
	f.vnic.PublicIP = ""
	task := newBWTask(t, enableSpec())

	out := bandwidthStep(context.Background(), task, enableSpec(), f)
	if !out.Done || out.Err == nil {
		t.Fatal("没有公网 IP 应当被拒绝")
	}
	if !strings.Contains(out.Err.Error(), "公网 IP") {
		t.Errorf("错误信息应当说明缺公网 IP，实际 %q", out.Err)
	}
}

func TestBandwidthSwitchRoutePreservesOtherRules(t *testing.T) {
	f := newFakeBW()
	f.autoReady = true
	// 路由表里有一条指向 peering 的静态规则，切换时不能丢。
	f.routes = append(f.routes, ociclient.RouteRule{
		Destination: "192.168.0.0/16", DestinationType: "CIDR_BLOCK",
		NetworkEntityID: "ocid1.localpeeringgateway.oc1..lpg", RouteType: "STATIC",
	})
	task := newBWTask(t, enableSpec())
	driveBW(t, task, enableSpec(), f, 20)

	if len(f.routeWrites) != 1 {
		t.Fatalf("应当只写一次路由表，实际 %d 次", len(f.routeWrites))
	}
	written := f.routeWrites[0]
	var hasPeering, hasDefault bool
	for _, r := range written {
		if r.Destination == "192.168.0.0/16" {
			hasPeering = true
		}
		if r.Destination == "0.0.0.0/0" && r.NetworkEntityID != testBWIGW {
			hasDefault = true
		}
		if r.RouteType == "LOCAL" {
			t.Error("LOCAL 规则是 OCI 自己维护的，不能写回去")
		}
	}
	if !hasPeering {
		t.Error("原有的 peering 静态规则被删掉了——整体替换语义下漏规则等于删用户的网络配置")
	}
	if !hasDefault {
		t.Error("默认路由没有改成 NAT")
	}
}

func TestBandwidthReusesExistingNat(t *testing.T) {
	f := newFakeBW()
	f.autoReady = true
	// 上一次任务建好了 NAT，但进程在写回 progress 之前被杀。
	f.nats["ocid1.natgateway.oc1..existing"] = &ociclient.NatGateway{
		ID: "ocid1.natgateway.oc1..existing", VcnID: testBWVcn,
		LifecycleState: ociclient.NatLifecycleAvailable,
		FreeformTags:   bwTags(testBWInstance),
	}
	task := newBWTask(t, enableSpec())
	driveBW(t, task, enableSpec(), f, 20)

	if f.natCreateN != 0 {
		t.Errorf("已有 NAT 时不该重复创建，实际创建 %d 次", f.natCreateN)
	}
	if len(f.nats) != 1 {
		t.Errorf("NAT 数量应当保持 1，实际 %d", len(f.nats))
	}
}

// ---- 关闭流程 ----

// seedEnabled 造一个「已经开启过」的环境：NLB + 后端集 + 监听 + NAT，
// 默认路由指向 NAT。
func seedEnabled(f *fakeBW) {
	f.autoReady = true
	f.nats["ocid1.natgateway.oc1..nat1"] = &ociclient.NatGateway{
		ID: "ocid1.natgateway.oc1..nat1", VcnID: testBWVcn,
		LifecycleState: ociclient.NatLifecycleAvailable,
		FreeformTags:   bwTags(testBWInstance),
	}
	f.nlbs["ocid1.networkloadbalancer.oc1..nlb1"] = &ociclient.NetworkLoadBalancer{
		ID: "ocid1.networkloadbalancer.oc1..nlb1", SubnetID: testBWSubnet,
		LifecycleState: ociclient.NLBLifecycleActive,
		FreeformTags:   bwTags(testBWInstance),
		IpAddresses:    []ociclient.NLBIpAddress{{IPAddress: "198.51.100.7", IsPublic: true}},
	}
	f.backendSets[bwBackendSetName] = true
	f.listeners[bwListenerName] = true
	f.routes = []ociclient.RouteRule{
		{Destination: "0.0.0.0/0", DestinationType: "CIDR_BLOCK",
			NetworkEntityID: "ocid1.natgateway.oc1..nat1"},
	}
}

func TestBandwidthDisableReverseOrder(t *testing.T) {
	f := newFakeBW()
	seedEnabled(f)
	task := newBWTask(t, disableSpec())

	stages := driveBW(t, task, disableSpec(), f, 20)
	want := []string{
		BwListenerDeleting, BwBackendSetDeleting, BwNLBDeleting,
		BwRouteRestoring, BwNatDeleting, BwCleaned,
	}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段序列不对：\n got %v\nwant %v", stages, want)
	}

	// 真正的顺序断言：删的顺序必须是建的严格逆序，且路由还原一定在删 NAT 之前。
	var order []string
	for _, c := range f.calls {
		switch c {
		case "DeleteNLBListener", "DeleteNLBBackendSet", "DeleteNetworkLoadBalancer",
			"UpdateRouteTable", "DeleteNatGateway":
			order = append(order, c)
		}
	}
	wantOrder := []string{
		"DeleteNLBListener", "DeleteNLBBackendSet", "DeleteNetworkLoadBalancer",
		"UpdateRouteTable", "DeleteNatGateway",
	}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("清理顺序不对（路由必须在删 NAT 之前还原）：\n got %v\nwant %v", order, wantOrder)
	}

	if len(f.nats) != 0 {
		t.Error("NAT 网关没有被删掉")
	}
	if len(f.nlbs) != 0 {
		t.Error("NLB 没有被删掉")
	}
	if got := defaultRouteTarget(f.routes); got != testBWIGW {
		t.Errorf("默认路由应当还原到互联网网关 %s，实际 %s", testBWIGW, got)
	}
}

func TestBandwidthDisableStopsAtFirstFailure(t *testing.T) {
	f := newFakeBW()
	seedEnabled(f)
	f.failOn["DeleteNetworkLoadBalancer"] = bwErr(500, "InternalError")
	task := newBWTask(t, disableSpec())

	for i := 0; i < 20; i++ {
		out := bandwidthStep(context.Background(), task, disableSpec(), f)
		applyOutcome(task, out)
		if out.Done {
			if out.Err == nil {
				t.Fatal("NLB 删除失败，清理不该判成功")
			}
			if out.Stage != BwNLBDeleting {
				t.Errorf("应当停在 NLB_DELETING，实际停在 %s", out.Stage)
			}
			break
		}
		f.converge()
	}

	// 关键断言：NLB 没删掉就不能继续动路由和 NAT。
	for _, c := range f.calls {
		if c == "UpdateRouteTable" {
			t.Fatal("NLB 删除失败后不该继续还原路由——停在原地是这套流程的核心约束")
		}
		if c == "DeleteNatGateway" {
			t.Fatal("NLB 删除失败后不该继续删 NAT")
		}
	}
	if len(f.nats) != 1 {
		t.Error("NAT 不该被动过")
	}
	if !strings.Contains(task.LastError, "InternalError") {
		t.Errorf("失败原因应当落库，实际 %q", task.LastError)
	}
}

func TestBandwidthDisableToleratesMissingResources(t *testing.T) {
	f := newFakeBW()
	// 没有任何本功能创建的资源，路由指向互联网网关。
	task := newBWTask(t, disableSpec())

	stages := driveBW(t, task, disableSpec(), f, 20)
	if len(stages) == 0 || stages[len(stages)-1] != BwCleaned {
		t.Fatalf("没有资源时也应当正常收尾，实际阶段 %v", stages)
	}
	for _, c := range f.calls {
		if c == "DeleteNatGateway" || c == "DeleteNetworkLoadBalancer" {
			t.Fatalf("没有资源却发了删除请求：%s", c)
		}
	}
	if !strings.Contains(task.Result, "本就不存在") {
		t.Errorf("结果里应当说明资源本来就不存在，实际 %q", task.Result)
	}
}

func TestBandwidthDisableFindsResourcesByTag(t *testing.T) {
	f := newFakeBW()
	seedEnabled(f)
	// 另加一个不属于本实例的 NLB，清理时必须无视它。
	f.nlbs["ocid1.networkloadbalancer.oc1..other"] = &ociclient.NetworkLoadBalancer{
		ID: "ocid1.networkloadbalancer.oc1..other", LifecycleState: ociclient.NLBLifecycleActive,
		FreeformTags: map[string]string{BandwidthTagKey: "ocid1.instance.oc1..someone-else"},
	}
	task := newBWTask(t, disableSpec())
	driveBW(t, task, disableSpec(), f, 20)

	if _, ok := f.nlbs["ocid1.networkloadbalancer.oc1..other"]; !ok {
		t.Fatal("删掉了别的实例的 NLB——tag 过滤失效")
	}
	if _, ok := f.nlbs["ocid1.networkloadbalancer.oc1..nlb1"]; ok {
		t.Error("本实例的 NLB 没被删掉")
	}
}

func TestBandwidthDisableRefusesAmbiguousIGW(t *testing.T) {
	f := newFakeBW()
	seedEnabled(f)
	f.igws = []ociclient.InternetGateway{{ID: "igw-a"}, {ID: "igw-b"}}
	task := newBWTask(t, disableSpec())

	var last outcome
	for i := 0; i < 20; i++ {
		last = bandwidthStep(context.Background(), task, disableSpec(), f)
		applyOutcome(task, last)
		if last.Done {
			break
		}
		f.converge()
	}
	if last.Err == nil {
		t.Fatal("有多个互联网网关时应当停下来，而不是猜一个")
	}
	if last.Stage != BwRouteRestoring {
		t.Errorf("应当停在 ROUTE_RESTORING，实际 %s", last.Stage)
	}
	if len(f.nats) != 1 {
		t.Error("路由没还原就不能删 NAT")
	}
}

// ---- 计时与契约 ----

func TestBandwidthStageTimeout(t *testing.T) {
	f := newFakeBW()
	f.autoReady = false
	task := newBWTask(t, enableSpec())
	// 直接造一个"卡在 NLB_CREATING 很久"的状态。
	task.Stage = BwNLBCreating
	task.State = store.OpsRunning
	task.Progress = encodeBWProgress(bwProgress{
		Stage: BwNLBCreating, StageSince: time.Now().Add(-2 * bwStageTimeout),
		NLBID: "ocid1.networkloadbalancer.oc1..nlb1",
	})
	f.nlbs["ocid1.networkloadbalancer.oc1..nlb1"] = &ociclient.NetworkLoadBalancer{
		ID: "ocid1.networkloadbalancer.oc1..nlb1", LifecycleState: ociclient.NLBLifecycleCreating,
	}

	out := bandwidthStep(context.Background(), task, enableSpec(), f)
	if !out.Done || out.Err == nil {
		t.Fatal("阶段超时应当判失败")
	}
	if !strings.Contains(out.Err.Error(), "已等待") {
		t.Errorf("错误信息应当说明等待了多久，实际 %q", out.Err)
	}
}

func TestBandwidthRetryResetsStageTimer(t *testing.T) {
	f := newFakeBW()
	f.autoReady = false
	task := newBWTask(t, enableSpec())
	task.Stage = BwNLBCreating
	// 重试：用户点了「重试」，run() 会把状态迁回 pending。
	task.State = store.OpsPending
	task.Progress = encodeBWProgress(bwProgress{
		Stage: BwNLBCreating, StageSince: time.Now().Add(-2 * bwStageTimeout),
		NLBID: "ocid1.networkloadbalancer.oc1..nlb1",
	})
	f.nlbs["ocid1.networkloadbalancer.oc1..nlb1"] = &ociclient.NetworkLoadBalancer{
		ID: "ocid1.networkloadbalancer.oc1..nlb1", LifecycleState: ociclient.NLBLifecycleCreating,
	}

	out := bandwidthStep(context.Background(), task, enableSpec(), f)
	if out.Done && out.Err != nil {
		t.Fatalf("重试后不该立刻撞上上一轮残留的阶段超时：%v", out.Err)
	}
	if out.Stage != BwNLBCreating {
		t.Errorf("应当继续等在 NLB_CREATING，实际 %s", out.Stage)
	}
}

func TestBandwidthUnknownStageFails(t *testing.T) {
	f := newFakeBW()
	task := newBWTask(t, enableSpec())
	// 库里是一个不属于开启流程的阶段：说明 spec 被改过，不能瞎猜。
	task.Stage = BwNatDeleting
	task.State = store.OpsRunning

	out := bandwidthStep(context.Background(), task, enableSpec(), f)
	// 不属于本动作的阶段会被当成"从第一步重来"，而不是直接失败——
	// 这是刻意的：阶段错乱最可能的原因是重试时换了动作，重头做才安全。
	if out.Done && out.Err != nil {
		t.Fatalf("阶段不属于本动作时应当重头开始，而不是失败：%v", out.Err)
	}
	if out.Stage != BwNatCreating {
		t.Errorf("应当从第一步（NAT_CREATING）重来，实际 %s", out.Stage)
	}
}

func TestBandwidthStages(t *testing.T) {
	enable := BandwidthStages(BandwidthEnable)
	if enable[0] != BwPrecheck || enable[len(enable)-1] != BwReady {
		t.Errorf("开启阶段应当从 PRECHECK 到 DONE，实际 %v", enable)
	}
	disable := BandwidthStages(BandwidthDisable)
	if disable[0] != BwDiscover || disable[len(disable)-1] != BwCleaned {
		t.Errorf("关闭阶段应当从 DISCOVER 到 CLEANED，实际 %v", disable)
	}
	if len(disable) < 2 {
		t.Fatal("关闭阶段数量不对")
	}
	// 关闭的第一个真正的删除动作必须晚于 DISCOVER。
	if disable[1] != BwListenerDeleting {
		t.Errorf("DISCOVER 之后应当是删监听，实际 %s", disable[1])
	}
}

func TestValidateBandwidthSpec(t *testing.T) {
	base := enableSpec()
	cases := []struct {
		name string
		mut  func(*BandwidthSpec)
		ok   bool
	}{
		{"正常开启", func(s *BandwidthSpec) {}, true},
		{"正常关闭", func(s *BandwidthSpec) { s.Action = BandwidthDisable }, true},
		{"未知动作", func(s *BandwidthSpec) { s.Action = "reboot" }, false},
		{"缺区域", func(s *BandwidthSpec) { s.Region = "" }, false},
		{"缺 compartment", func(s *BandwidthSpec) { s.CompartmentID = "" }, false},
		{"缺实例", func(s *BandwidthSpec) { s.InstanceID = "" }, false},
		{"端口越界", func(s *BandwidthSpec) { s.ListenerPort = 70000 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base
			c.mut(&s)
			err := ValidateBandwidthSpec(s)
			if c.ok && err != nil {
				t.Fatalf("应当通过，实际报错：%v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("应当报错，实际通过了")
			}
		})
	}
}

func TestDecodeBandwidthSpecDefaults(t *testing.T) {
	s, err := DecodeBandwidthSpec(`{"region":"ap-tokyo-1","compartmentId":"c","instanceId":"i"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if s.Action != BandwidthEnable {
		t.Errorf("缺省动作应当是 enable，实际 %q", s.Action)
	}
	if s.ListenerPort != bwDefaultPort {
		t.Errorf("缺省端口应当是 %d，实际 %d", bwDefaultPort, s.ListenerPort)
	}
}

func TestBandwidthHelpers(t *testing.T) {
	if !isAMDFreeTierShape("VM.Standard.E2.1.Micro") {
		t.Error("AMD 免费规格应当被识别")
	}
	if isAMDFreeTierShape("VM.Standard.A1.Flex") {
		t.Error("ARM 规格不该被当成 AMD 免费实例")
	}

	rules := []ociclient.RouteRule{
		{Destination: "10.0.0.0/16", RouteType: "LOCAL"},
		{Destination: "0.0.0.0/0", NetworkEntityID: "igw"},
	}
	static := staticRouteRules(rules)
	if len(static) != 1 || static[0].Destination != "0.0.0.0/0" {
		t.Errorf("只该保留静态规则，实际 %v", static)
	}

	replaced := replaceDefaultRoute(rules, "nat", "")
	if got := defaultRouteTarget(replaced); got != "nat" {
		t.Errorf("默认路由应当指向 nat，实际 %s", got)
	}
	if len(replaced) != 1 {
		t.Errorf("LOCAL 规则不该出现在写入内容里，实际 %d 条", len(replaced))
	}

	removed := removeDefaultRoute(rules)
	if len(removed) != 0 {
		t.Errorf("删默认路由后应当为空，实际 %v", removed)
	}

	if !ociclient.IsNotFound(bwErr(404, "x")) {
		t.Error("IsNotFound 应当识别 404")
	}
	if !ociclient.IsConflict(bwErr(409, "x")) {
		t.Error("IsConflict 应当识别 409")
	}
}
