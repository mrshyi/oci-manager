package opssvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ocicore/internal/cf"
	"ocicore/internal/netsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// fakeIPChanger 按调用顺序返回预设的 IP 或错误。
type fakeIPChanger struct {
	ips   []string
	errs  []error
	calls int
}

func (f *fakeIPChanger) ChangePublicIP(_ context.Context, _, _, _ string) (*netsvc.ChangePublicIPResult, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	ip := ""
	if i < len(f.ips) {
		ip = f.ips[i]
	}
	return &netsvc.ChangePublicIPResult{OldIP: "198.51.100.1", NewIP: ip}, nil
}

// 第 3 轮命中目标网段就停止：前两轮排下一轮，第三轮成功。
func TestChangeIPStopsWhenCIDRHitOnThirdRound(t *testing.T) {
	f := &fakeIPChanger{ips: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}}
	spec := ChangeIPSpec{
		Region: "r", VnicID: "v", CompartmentID: "c",
		TargetCIDRs: []string{"3.3.3.0/24"},
	}
	task := &store.OpsTask{Attempts: 0, MaxAttempts: 30, IntervalSeconds: 20}

	o1 := changeIPTowards(context.Background(), task, spec, f, nil)
	if o1.Done {
		t.Fatalf("第 1 轮不应结束: %+v", o1)
	}
	if o1.NextAt.IsZero() {
		t.Error("未命中时应排下一轮")
	}

	task.Attempts = 1
	o2 := changeIPTowards(context.Background(), task, spec, f, nil)
	if o2.Done {
		t.Fatalf("第 2 轮不应结束: %+v", o2)
	}

	task.Attempts = 2
	o3 := changeIPTowards(context.Background(), task, spec, f, nil)
	if !o3.Done || o3.Err != nil {
		t.Fatalf("第 3 轮应成功结束: %+v", o3)
	}
	if !strings.Contains(o3.Result, "3.3.3.3") || !strings.Contains(o3.Result, "第 3 轮") {
		t.Errorf("结果文案不符合预期: %s", o3.Result)
	}
	if f.calls != 3 {
		t.Errorf("换 IP 调用次数 = %d，期望 3", f.calls)
	}
}

// 到达最大尝试次数就停止，且不再发换 IP 请求（旧 IP 已被换掉，不该再动）。
func TestChangeIPStopsAtMaxAttempts(t *testing.T) {
	f := &fakeIPChanger{}
	task := &store.OpsTask{Attempts: 30, MaxAttempts: 30}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}

	o := changeIPTowards(context.Background(), task, spec, f, nil)
	if !o.Done || o.Err == nil {
		t.Fatalf("应失败结束: %+v", o)
	}
	if f.calls != 0 {
		t.Errorf("超限后不应再调用换 IP，实际 %d 次", f.calls)
	}
	if !strings.Contains(o.Err.Error(), "保留不变") {
		t.Errorf("应说明当前 IP 保留不变: %v", o.Err)
	}
}

// 保留 IP 不可自动更换，直接判失败。
func TestChangeIPRejectsReservedIP(t *testing.T) {
	f := &fakeIPChanger{errs: []error{netsvc.ErrReservedIP}}
	task := &store.OpsTask{Attempts: 0, MaxAttempts: 30}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}

	o := changeIPTowards(context.Background(), task, spec, f, nil)
	if !o.Done || o.Err == nil {
		t.Fatalf("保留 IP 应直接失败: %+v", o)
	}
	if !strings.Contains(o.Err.Error(), "保留 IP") {
		t.Errorf("错误文案应点明保留 IP: %v", o.Err)
	}
	if f.calls != 1 {
		t.Errorf("调用次数 = %d，期望 1", f.calls)
	}
}

// 429 只退避，不计入尝试次数，也不结束任务。
func TestChangeIPThrottleDoesNotCountAttempt(t *testing.T) {
	apiErr := &ociclient.APIError{StatusCode: 429, Class: ociclient.ClassThrottled}
	f := &fakeIPChanger{errs: []error{apiErr}}
	task := &store.OpsTask{Attempts: 0, MaxAttempts: 30, IntervalSeconds: 20}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}

	o := changeIPTowards(context.Background(), task, spec, f, nil)
	if o.Done {
		t.Fatalf("限流不应结束任务: %+v", o)
	}
	if !o.NoAttempt {
		t.Error("限流退避必须标记 NoAttempt，避免耗尽尝试次数")
	}
	if o.NextAt.IsZero() {
		t.Error("限流应排下一轮")
	}
	if !strings.Contains(o.Err.Error(), "限流") {
		t.Errorf("错误文案应说明限流: %v", o.Err)
	}
}

// 超过最长运行时间就停止，且不再发换 IP 请求。
func TestChangeIPRespectsDeadline(t *testing.T) {
	f := &fakeIPChanger{}
	task := &store.OpsTask{
		Attempts: 0, MaxAttempts: 30,
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}

	o := changeIPTowards(context.Background(), task, spec, f, nil)
	if !o.Done || o.Err == nil {
		t.Fatalf("超时应失败结束: %+v", o)
	}
	if f.calls != 0 {
		t.Errorf("超时后不应再调用换 IP，实际 %d 次", f.calls)
	}
}

func TestMatchesAnyCIDR(t *testing.T) {
	cases := []struct {
		ip   string
		cidr string
		want bool
	}{
		{"1.2.3.4", "1.2.3.0/24", true},
		{"1.2.4.4", "1.2.3.0/24", false},
		{"203.0.113.9", "203.0.113.0/24", true},
		{"not-an-ip", "1.2.3.0/24", false},
	}
	for _, c := range cases {
		if got := matchesAnyCIDR(c.ip, []string{c.cidr}); got != c.want {
			t.Errorf("matchesAnyCIDR(%q,%q) = %v，期望 %v", c.ip, c.cidr, got, c.want)
		}
	}
}

func TestValidateChangeIPSpec(t *testing.T) {
	good := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"1.2.3.0/24"}}
	if err := ValidateChangeIPSpec(good); err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
	bad := []ChangeIPSpec{
		{VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"1.2.3.0/24"}},
		{Region: "r", CompartmentID: "c", TargetCIDRs: []string{"1.2.3.0/24"}},
		{Region: "r", VnicID: "v", TargetCIDRs: []string{"1.2.3.0/24"}},
		{Region: "r", VnicID: "v", CompartmentID: "c"},
		{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"bad-cidr"}},
	}
	for i, s := range bad {
		if err := ValidateChangeIPSpec(s); err == nil {
			t.Errorf("case %d 期望报错", i)
		}
	}
}

// 验证 NoAttempt 真的不增加 attempts（限流退避不能耗尽次数）。
func TestRescheduleNoAttemptDoesNotIncrement(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)
	svc := New(Deps{Store: st})

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: store.OpsKindChangeIP, AccountID: acc.ID, Name: "换 IP",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 限流退避：不计次。
	svc.reschedule(context.Background(), task, outcome{
		Done: false, NoAttempt: true,
		NextAt: time.Now().Add(time.Minute), Err: errors.New("限流"),
	})
	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 0 {
		t.Fatalf("限流退避后 attempts = %d，期望 0", got.Attempts)
	}

	// 正常一轮：计次。
	svc.reschedule(context.Background(), got, outcome{
		Done: false, NextAt: time.Now().Add(time.Minute),
	})
	got2, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Attempts != 1 {
		t.Fatalf("正常一轮后 attempts = %d，期望 1", got2.Attempts)
	}
}

/* ---------- 换 IP 与 DNS 联动的边界 ---------- */

// fakeDNS 记录被调用的次数与最后一次拿到的 IP。
type fakeDNS struct {
	status string
	detail string
	ips    []string
}

func (f *fakeDNS) UpdateForInstance(_ context.Context, _, _, ip string) cf.Result {
	f.ips = append(f.ips, ip)
	if f.status == "" {
		// 模拟「这台实例没有绑定任何记录」。
		return cf.Result{}
	}
	return cf.Result{Status: f.status, Detail: f.detail, Records: 1}
}

// 每一轮都要同步：旧 IP 已经释放，域名指着它就是一个打不开的站。
func TestChangeIPSyncsDNSEveryRound(t *testing.T) {
	f := &fakeIPChanger{ips: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}}
	dns := &fakeDNS{status: cf.StatusUpdated}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}
	task := &store.OpsTask{InstanceID: "inst-1", MaxAttempts: 30, IntervalSeconds: 20}

	for i := 0; i < 3; i++ {
		task.Attempts = i
		changeIPTowards(context.Background(), task, spec, f, dns)
	}

	if len(dns.ips) != 3 {
		t.Fatalf("DNS 同步次数 = %d，期望 3（每轮都同步）", len(dns.ips))
	}
	if dns.ips[2] != "3.3.3.3" {
		t.Errorf("最后一轮应同步 3.3.3.3，实际 %q", dns.ips[2])
	}
}

// DNS 更新失败不能让换 IP 判失败：IP 确实换了，机器是通的。
func TestChangeIPDNSFailureDoesNotFailTask(t *testing.T) {
	f := &fakeIPChanger{ips: []string{"3.3.3.3"}}
	dns := &fakeDNS{status: cf.StatusFailed, detail: "a.example.com：API token 无效"}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}
	task := &store.OpsTask{InstanceID: "inst-1", MaxAttempts: 30}

	o := changeIPTowards(context.Background(), task, spec, f, dns)

	if !o.Done {
		t.Fatalf("换 IP 命中后应结束: %+v", o)
	}
	if o.Err != nil {
		t.Fatalf("DNS 失败不该让任务失败: %v", o.Err)
	}
	if o.Result == "" {
		t.Error("换 IP 的结果仍应写出来")
	}
	if o.DNSStatus != cf.StatusFailed {
		t.Errorf("DNSStatus = %q，期望 %q", o.DNSStatus, cf.StatusFailed)
	}
	if !strings.Contains(o.DNSError, "token") {
		t.Errorf("DNSError 应带上原因: %q", o.DNSError)
	}
	if strings.Contains(o.Result, "token") {
		t.Error("DNS 的失败原因不该混进换 IP 的结果里")
	}
}

// DNS 成功时不留原因：界面不该显示「已更新」外加一句像错误的话。
func TestChangeIPDNSSuccessLeavesNoError(t *testing.T) {
	f := &fakeIPChanger{ips: []string{"3.3.3.3"}}
	dns := &fakeDNS{status: cf.StatusUpdated, detail: "1 条记录已指向 3.3.3.3"}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}
	task := &store.OpsTask{InstanceID: "inst-1", MaxAttempts: 30}

	o := changeIPTowards(context.Background(), task, spec, f, dns)
	if o.DNSStatus != cf.StatusUpdated {
		t.Fatalf("DNSStatus = %q", o.DNSStatus)
	}
	if o.DNSError != "" {
		t.Errorf("成功时不该有 DNSError: %q", o.DNSError)
	}
}

// 没接线（upd == nil）时换 IP 照常，且不产生任何 DNS 字段。
func TestChangeIPWithoutDNSUpdater(t *testing.T) {
	f := &fakeIPChanger{ips: []string{"3.3.3.3"}}
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}
	task := &store.OpsTask{InstanceID: "inst-1", MaxAttempts: 30}

	o := changeIPTowards(context.Background(), task, spec, f, nil)
	if !o.Done || o.Err != nil {
		t.Fatalf("应成功: %+v", o)
	}
	if o.DNSStatus != "" || o.DNSError != "" {
		t.Errorf("没接线时不该有 DNS 字段: %q / %q", o.DNSStatus, o.DNSError)
	}
}

// 没有绑定时不写 DNS 字段：每个换 IP 任务都多一行「DNS：跳过」纯属噪音。
func TestChangeIPSkipsDNSFieldsWhenNotInvolved(t *testing.T) {
	f := &fakeIPChanger{ips: []string{"3.3.3.3"}}
	dns := &fakeDNS{} // status 为空 = 没有绑定
	spec := ChangeIPSpec{Region: "r", VnicID: "v", CompartmentID: "c", TargetCIDRs: []string{"3.3.3.0/24"}}
	task := &store.OpsTask{InstanceID: "inst-1", MaxAttempts: 30}

	o := changeIPTowards(context.Background(), task, spec, f, dns)
	if o.DNSStatus != "" {
		t.Errorf("没有绑定时不该写 DNSStatus: %q", o.DNSStatus)
	}
}

// 落库层面：result 与 dns_* 是两列，换 IP 成功 + DNS 失败时状态仍是 succeeded。
func TestFinishKeepsTaskSuccessWhenDNSFails(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)
	svc := New(Deps{Store: st})

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: store.OpsKindChangeIP, AccountID: acc.ID, Name: "换 IP", InstanceID: "inst-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(t.Context(), task.ID, store.OpsRunning); err != nil {
		t.Fatal(err)
	}

	svc.finish(context.Background(), task, store.OpsSucceeded, outcome{
		Done:      true,
		Result:    "第 3 轮命中目标网段：198.51.100.1 → 3.3.3.3",
		DNSStatus: cf.StatusFailed,
		DNSError:  "a.example.com：API token 无效",
	})

	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.OpsSucceeded {
		t.Fatalf("任务状态 = %q，DNS 失败不该改变它", got.State)
	}
	if !strings.Contains(got.Result, "3.3.3.3") {
		t.Errorf("换 IP 的结果丢了: %q", got.Result)
	}
	if got.LastError != "" {
		t.Errorf("DNS 失败不该写进 last_error（那是任务失败的原因）: %q", got.LastError)
	}
	if got.DNSStatus != cf.StatusFailed {
		t.Errorf("dns_status = %q，期望 %q", got.DNSStatus, cf.StatusFailed)
	}
	if !strings.Contains(got.DNSError, "token") {
		t.Errorf("dns_error = %q", got.DNSError)
	}
}

// 上一轮 DNS 失败、这一轮成功时，原因必须被清掉。
func TestDNSStatusRewriteClearsStaleError(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: store.OpsKindChangeIP, AccountID: acc.ID, Name: "换 IP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceOpsTask(t.Context(), task.ID, store.OpsTaskProgress{
		DNSStatus: cf.StatusFailed, DNSError: "旧的原因", NextAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceOpsTask(t.Context(), task.ID, store.OpsTaskProgress{
		DNSStatus: cf.StatusUpdated, DNSError: "",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DNSError != "" {
		t.Fatalf("陈旧的失败原因没被清掉: %q", got.DNSError)
	}
}
