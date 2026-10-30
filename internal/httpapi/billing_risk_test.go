package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ocicore/internal/accountsvc"
	"ocicore/internal/ociclient"
)

// billingLimit 是一项限额的假数据。
type billingLimit struct {
	value int64
	used  int64
	// ad 为空表示 REGION 作用域；非空则让 fetchQuotaItem 走 AD 分支。
	ad string
}

// fakeOCIBilling 冒充计费风险要用到的四个 OCI 接口。
//
// 只做一件事：按路径返回事先摆好的数字。判定逻辑全在 accountsvc 的
// 纯函数里，那部分已经用表驱动测过了；这里要验的是「取数有没有接错」——
// 比如把公网 IP 的 scope 传反、把 NLB 的已删除实例也算进去。
type fakeOCIBilling struct {
	srv *httptest.Server
	mu  sync.Mutex

	limits map[string]billingLimit
	// deny 里的子串命中请求路径就返回 403，用来复现「这项没查到」。
	deny []string

	publicIPs string
	nlbs      string
}

func newFakeOCIBilling(t *testing.T) *fakeOCIBilling {
	t.Helper()
	f := &fakeOCIBilling{
		limits:    map[string]billingLimit{},
		publicIPs: `[]`,
		nlbs:      `[]`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// setLimits 摆好三项限额。used 大于 value 即视为超额。
func (f *fakeOCIBilling) setLimits(arm, micro, block billingLimit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits[ociclient.LimitARMCores] = arm
	f.limits[ociclient.LimitE2MicroVMs] = micro
	f.limits[ociclient.LimitStorageGB] = block
}

func (f *fakeOCIBilling) denyPath(substr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deny = append(f.deny, substr)
}

func (f *fakeOCIBilling) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	deny := append([]string(nil), f.deny...)
	limits := make(map[string]billingLimit, len(f.limits))
	for k, v := range f.limits {
		limits[k] = v
	}
	ips, nlbs := f.publicIPs, f.nlbs
	f.mu.Unlock()

	path := r.URL.Path
	for _, d := range deny {
		if strings.Contains(path, d) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"NotAuthorizedOrNotFound","message":"not authorized"}`))
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(path, "/limitValues"):
		name := r.URL.Query().Get("name")
		l, ok := limits[name]
		if !ok {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		scope := "REGION"
		if l.ad != "" {
			scope = "AD"
		}
		_, _ = w.Write([]byte(`[{"name":"` + name + `","scopeType":"` + scope +
			`","availabilityDomain":"` + l.ad + `","value":` + jsonInt(l.value) + `}]`))

	case strings.HasSuffix(path, "/resourceAvailability"):
		// /services/{svc}/limits/{name}/resourceAvailability
		parts := strings.Split(strings.Trim(path, "/"), "/")
		var name string
		if len(parts) >= 2 {
			name = parts[len(parts)-2]
		}
		l := limits[name]
		_, _ = w.Write([]byte(`{"used":` + jsonInt(l.used) +
			`,"available":` + jsonInt(l.value-l.used) + `}`))

	case strings.HasSuffix(path, "/publicIps"):
		_, _ = w.Write([]byte(ips))

	case strings.HasSuffix(path, "/networkLoadBalancers"):
		_, _ = w.Write([]byte(nlbs))

	case strings.HasSuffix(path, "/usage"):
		// 空的用量结果：出站流量按 0 算，不产生条目。
		_, _ = w.Write([]byte(`{"items":[]}`))

	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

// itoa 把数字拼进 JSON。本文件里全是 int64，标准库的 FormatInt 最直接。
func jsonInt(v int64) string { return strconv.FormatInt(v, 10) }

// 这是 P2-4 的验收条件：超规格 ARM + NLB + 保留 IP，三条都要报出来，
// 且金额与规则里的单价对得上。
func TestBillingRiskReportsCharges(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "billrisk")

	fake := newFakeOCIBilling(t)
	// ARM 用 4 个 OCPU、免费额度 2 个：超 2 个。
	// Micro 与块存储都在额度内，不该出现在结果里。
	fake.setLimits(
		billingLimit{value: 2, used: 4},
		billingLimit{value: 2, used: 1, ad: "Uocm:AP-TOKYO-1-AD-1"},
		billingLimit{value: 200, used: 150},
	)
	// 2 个保留 IP + 1 个临时 IP：临时的不计费，不能被算进去。
	fake.publicIPs = `[
		{"id":"ip1","lifetime":"RESERVED","lifecycleState":"AVAILABLE"},
		{"id":"ip2","lifetime":"RESERVED","lifecycleState":"AVAILABLE"},
		{"id":"ip3","lifetime":"EPHEMERAL","lifecycleState":"ASSIGNED"}
	]`
	// 1 个 ACTIVE + 1 个 DELETED：删除中的仍在计费，已删除的不算。
	fake.nlbs = `[
		{"id":"nlb1","displayName":"web","lifecycleState":"ACTIVE"},
		{"id":"nlb2","displayName":"old","lifecycleState":"DELETED"}
	]`
	h.api.conns.SetEndpointResolver(func(ociclient.Service, string) string { return fake.srv.URL })

	resp := h.get("/api/accounts/" + id + "/billing-risk")
	expectStatus(t, resp, http.StatusOK)
	risk := decode[accountsvc.BillingRisk](t, resp)

	if len(risk.Items) != 3 {
		t.Fatalf("应报 3 项，实际 %d 项: %+v", len(risk.Items), risk.Items)
	}
	// 顺序固定：ARM → NLB → 保留 IP。
	wantKeys := []string{
		accountsvc.RiskARMOver, accountsvc.RiskNLB, accountsvc.RiskReservedPublicIP,
	}
	for i, want := range wantKeys {
		if risk.Items[i].Key != want {
			t.Errorf("第 %d 项 key = %q，期望 %q", i, risk.Items[i].Key, want)
		}
		if risk.Items[i].Severity != accountsvc.RiskBilling {
			t.Errorf("%s 的严重度 = %q，期望 billing", want, risk.Items[i].Severity)
		}
		if !risk.Items[i].Confirmed {
			t.Errorf("%s 的额度来自 limits 接口，应标为已确认", want)
		}
	}

	// 金额与单价对得上：ARM 超 2 OCPU → 2 × $0.01 × 24 × 7.2 = ¥3.46；
	// NLB 1 个 → $0.0225 × 24 × 7.2 = ¥3.89；保留 IP 2 个 → ¥0.86。
	byKey := map[string]accountsvc.BillingRiskItem{}
	for _, it := range risk.Items {
		byKey[it.Key] = it
	}
	for key, want := range map[string]float64{
		accountsvc.RiskARMOver:          3.46,
		accountsvc.RiskNLB:              3.89,
		accountsvc.RiskReservedPublicIP: 0.86,
	} {
		if got := byKey[key].EstDailyCNY; got != want {
			t.Errorf("%s 的日估算 = %v，期望 %v", key, got, want)
		}
	}
	if risk.TotalDailyCNY != 8.21 {
		t.Errorf("日总额 = %v，期望 8.21", risk.TotalDailyCNY)
	}
	if risk.TotalMonthlyCNY != 246.3 {
		t.Errorf("月总额 = %v，期望 246.3", risk.TotalMonthlyCNY)
	}
	// 估算两个字必须出现在响应里，前端才可能显示出来。
	if !strings.Contains(risk.Notice, "估算") {
		t.Errorf("整体说明应包含「估算」，实际 %q", risk.Notice)
	}
	if risk.Region != "ap-tokyo-1" {
		t.Errorf("区域 = %q", risk.Region)
	}
}

// 全在免费额度内时必须一条都不报。
//
// 这一条是反向保护：误报一次，用户就会开始无视这个面板。
func TestBillingRiskQuietWhenWithinFreeTier(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "billquiet")

	fake := newFakeOCIBilling(t)
	fake.setLimits(
		billingLimit{value: 4, used: 2},
		billingLimit{value: 2, used: 1, ad: "Uocm:AP-TOKYO-1-AD-1"},
		billingLimit{value: 200, used: 47},
	)
	fake.publicIPs = `[{"id":"ip1","lifetime":"EPHEMERAL","lifecycleState":"ASSIGNED"}]`
	h.api.conns.SetEndpointResolver(func(ociclient.Service, string) string { return fake.srv.URL })

	resp := h.get("/api/accounts/" + id + "/billing-risk")
	expectStatus(t, resp, http.StatusOK)
	risk := decode[accountsvc.BillingRisk](t, resp)

	if len(risk.Items) != 0 {
		t.Fatalf("额度内不该报任何项，实际 %+v", risk.Items)
	}
	if risk.HasBilling() {
		t.Error("额度内不该判为在计费")
	}
	if risk.TotalDailyCNY != 0 {
		t.Errorf("总额 = %v，期望 0", risk.TotalDailyCNY)
	}
}

// 查不到的项要明说，不能返回一个空的「一切正常」。
//
// 缺 limits 权限是真实账号最常见的状况——免费账号默认没有 read limits，
// 而这时如果返回 items: []，用户会以为账单干净。
func TestBillingRiskSurfacesFetchErrors(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "billfail")

	fake := newFakeOCIBilling(t)
	fake.denyPath("/limitValues")
	fake.denyPath("/publicIps")
	fake.denyPath("/usage")
	h.api.conns.SetEndpointResolver(func(ociclient.Service, string) string { return fake.srv.URL })

	resp := h.get("/api/accounts/" + id + "/billing-risk")
	expectStatus(t, resp, http.StatusOK)
	risk := decode[accountsvc.BillingRisk](t, resp)

	byKey := map[string]accountsvc.BillingRiskItem{}
	for _, it := range risk.Items {
		byKey[it.Key] = it
	}
	// 三项限额 + 保留 IP + 出站流量共五路失败，都要有交代。
	for _, key := range []string{
		accountsvc.RiskARMOver, accountsvc.RiskMicroOver, accountsvc.RiskBlockOver,
		accountsvc.RiskReservedPublicIP, accountsvc.RiskCrossRegionEgress,
	} {
		it, ok := byKey[key]
		if !ok {
			t.Errorf("%s 取数失败时应出一条「没查到」", key)
			continue
		}
		if it.Severity != accountsvc.RiskAttention {
			t.Errorf("%s 的严重度 = %q，没查到只能是 attention", key, it.Severity)
		}
		if it.EstDailyCNY != 0 {
			t.Errorf("%s 没查到却给了金额 %v", key, it.EstDailyCNY)
		}
		if !strings.Contains(it.Detail, "无法确认") {
			t.Errorf("%s 的说明应写清是没查到，实际 %q", key, it.Detail)
		}
	}
	// 没查到不等于在扣钱，不能把账号标成计费告警。
	if risk.HasBilling() {
		t.Error("取数失败不该判为在计费")
	}
	// NLB 那一路是通的（返回空列表），不该被误报。
	if _, ok := byKey[accountsvc.RiskNLB]; ok {
		t.Error("NLB 查询成功且为 0，不该报出来")
	}
}

// 停用的账号查不到任何东西，但接口本身仍要 200——
// 它是只读查询，停用只影响能不能打 OCI。
func TestBillingRiskDisabledAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "billoff")

	// 仍然指向假服务端：即便账号停用了，也不能让测试真去打 OCI。
	fake := newFakeOCIBilling(t)
	h.api.conns.SetEndpointResolver(func(ociclient.Service, string) string { return fake.srv.URL })

	resp := h.do("PATCH", "/api/accounts/"+id, map[string]any{"enabled": false}, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = h.get("/api/accounts/" + id + "/billing-risk")
	expectStatus(t, resp, http.StatusOK)
	risk := decode[accountsvc.BillingRisk](t, resp)

	var found bool
	for _, it := range risk.Items {
		if it.Key == accountsvc.RiskCrossRegionEgress {
			found = true
			if !strings.Contains(it.Detail, "停用") {
				t.Errorf("应说明账号已停用，实际 %q", it.Detail)
			}
		}
	}
	if !found {
		t.Error("停用账号的出站流量应报「查不到」")
	}
}

func TestBillingRiskRequiresAuth(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/api/accounts/some-id/billing-risk")
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestBillingRiskUnknownAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/accounts/does-not-exist/billing-risk")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}
