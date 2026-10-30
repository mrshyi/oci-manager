package accountsvc

import (
	"strings"
	"testing"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// riskInput 造一个"什么都没超"的基线，各用例只改自己关心的那一项。
func riskInput() RiskInput {
	return RiskInput{
		Account:      store.Account{ID: "acc1", DefaultRegion: "ap-tokyo-1"},
		ARM:          LimitSnapshot{Used: 1, Limit: 2, Known: true},
		Micro:        LimitSnapshot{Used: 1, Limit: 2, Known: true},
		Block:        LimitSnapshot{Used: 100, Limit: 200, Known: true},
		FreeEgressGB: 10 * 1024,
		EgressKnown:  true,
		EgressGB:     100,
		Errors:       map[string]string{},
	}
}

func riskKeys(r BillingRisk) map[string]BillingRiskItem {
	out := make(map[string]BillingRiskItem, len(r.Items))
	for _, it := range r.Items {
		out[it.Key] = it
	}
	return out
}

// 全部在额度内时不该报任何东西。
//
// 一屏"没有风险"和一屏假警报一样没用——后者会让用户学会无视这个页面。
func TestAssessBillingRiskQuietWhenWithinFreeTier(t *testing.T) {
	risk := AssessBillingRisk(riskInput())
	if len(risk.Items) != 0 {
		t.Errorf("没有风险时不该输出条目，实际 %+v", risk.Items)
	}
	if risk.TotalDailyCNY != 0 {
		t.Errorf("总费用 = %v，期望 0", risk.TotalDailyCNY)
	}
	if risk.HasBilling() {
		t.Error("不该判为在计费")
	}
	// 说明里必须有「估算」二字：数字再准也是粗算的。
	if !strings.Contains(risk.Notice, "估算") {
		t.Errorf("整体说明应标注估算，实际 %q", risk.Notice)
	}
}

// 这是 P2-4 的验收条件：开一台超规格 ARM，要出现「ARM 超出免费额度，预计 ¥X/日」。
func TestAssessBillingRiskARMOverQuota(t *testing.T) {
	in := riskInput()
	in.ARM = LimitSnapshot{Used: 4, Limit: 2, Known: true}

	risk := AssessBillingRisk(in)
	it, ok := riskKeys(risk)[RiskARMOver]
	if !ok {
		t.Fatalf("应报出 ARM 超额，实际 %+v", risk.Items)
	}
	if it.Severity != RiskBilling {
		t.Errorf("严重度 = %q，期望 billing", it.Severity)
	}
	if it.EstDailyCNY <= 0 {
		t.Errorf("应给出正的估算日费用，实际 %v", it.EstDailyCNY)
	}
	if !strings.Contains(it.Label, "ARM") || !strings.Contains(it.Label, "超出免费额度") {
		t.Errorf("标题应写明「ARM 超出免费额度」，实际 %q", it.Label)
	}
	// Detail 要有具体数字，只说"超额了"没法让人判断该关哪台。
	if !strings.Contains(it.Detail, "4") || !strings.Contains(it.Detail, "2") {
		t.Errorf("详情应带上实际用量与额度，实际 %q", it.Detail)
	}
	// 估算依据必须给出来。
	if !strings.Contains(it.Basis, "OCPU") {
		t.Errorf("应说明估算依据，实际 %q", it.Basis)
	}
	if !it.Confirmed {
		t.Error("额度来自 limits 接口时应标记为已确认")
	}

	// 2 个 OCPU 超出 × $0.01/h × 24h × 7.2 ≈ 3.46 元/日
	if it.EstDailyCNY < 3.4 || it.EstDailyCNY > 3.5 {
		t.Errorf("估算日费用 = %v，期望约 3.46", it.EstDailyCNY)
	}
}

// 拿不到 limits 时，只能用公开常量提示，且必须降级为「需要看一眼」而不是
// 「你在扣钱」，并且不给金额——数字站不住脚就不要给。
func TestAssessBillingRiskARMWithoutLimitsIsAdvisory(t *testing.T) {
	in := riskInput()
	in.ARM = LimitSnapshot{Known: false}
	// 本地实例表里 3 台 A1，每台 1 OCPU，超过公开额度的 2。
	in.Instances = []store.Instance{
		{Shape: "VM.Standard.A1.Flex", Ocpus: 1},
		{Shape: "VM.Standard.A1.Flex", Ocpus: 1},
		{Shape: "VM.Standard.A1.Flex", Ocpus: 1},
	}

	risk := AssessBillingRisk(in)
	it, ok := riskKeys(risk)[RiskARMOver]
	if !ok {
		t.Fatalf("应报出 ARM 超额提示，实际 %+v", risk.Items)
	}
	if it.Severity != RiskAttention {
		t.Errorf("严重度 = %q，期望 attention（额度不可信时不能断言在扣钱）", it.Severity)
	}
	if it.EstDailyCNY != 0 {
		t.Errorf("额度不可信时不该给金额，实际 %v", it.EstDailyCNY)
	}
	if it.Confirmed {
		t.Error("额度来自公开常量时不该标为已确认")
	}
	if !strings.Contains(it.Detail, "公开免费额度") {
		t.Errorf("详情应说明额度来源是公开常量，实际 %q", it.Detail)
	}
	if risk.HasBilling() {
		t.Error("仅凭公开常量不该判为「在计费」")
	}
}

// 本地兜底只统计 A1 形状，别把 E2/E3/E4 的 OCPU 也算进 ARM 额度里。
func TestLocalARMSnapshotIgnoresNonA1(t *testing.T) {
	instances := []store.Instance{
		{Shape: "VM.Standard.A1.Flex", Ocpus: 2},
		{Shape: "VM.Standard.E2.1.Micro", Ocpus: 0.125},
		{Shape: "VM.Standard.E4.Flex", Ocpus: 8},
	}
	snap := localARMSnapshot(instances)
	if snap.Used != 2 {
		t.Errorf("ARM 用量 = %d，期望 2（只算 A1）", snap.Used)
	}
	if snap.Known {
		t.Error("本地兜底的快照不该标记为已知")
	}
	if snap.Limit != ociclient.AlwaysFreeARMOcpus {
		t.Errorf("兜底额度 = %d，期望公开常量 %d", snap.Limit, ociclient.AlwaysFreeARMOcpus)
	}
}

func TestLocalMicroSnapshot(t *testing.T) {
	instances := []store.Instance{
		{Shape: "VM.Standard.E2.1.Micro"},
		{Shape: "VM.Standard.E2.1.Micro"},
		{Shape: "VM.Standard.E2.1.Micro"},
		{Shape: "VM.Standard.A1.Flex"},
	}
	snap := localMicroSnapshot(instances)
	if snap.Used != 3 {
		t.Errorf("Micro 台数 = %d，期望 3", snap.Used)
	}
}

// 块存储的本地兜底一定偏低（块卷不在实例表里），所以文案必须说明这一点。
func TestLocalBlockSnapshotExcludesBlockVolumes(t *testing.T) {
	instances := []store.Instance{
		{BootVolumeGB: 50}, {BootVolumeGB: 100},
	}
	snap := localBlockSnapshot(instances)
	if snap.Used != 150 {
		t.Errorf("引导卷总量 = %d，期望 150", snap.Used)
	}

	in := riskInput()
	in.Block = LimitSnapshot{Known: false}
	in.Instances = instances
	// 150 没超过 200，不报。
	if _, ok := riskKeys(AssessBillingRisk(in))[RiskBlockOver]; ok {
		t.Error("没超额时不该报")
	}

	// 250 GB 超过 200。
	in.Instances = []store.Instance{{BootVolumeGB: 250}}
	it, ok := riskKeys(AssessBillingRisk(in))[RiskBlockOver]
	if !ok {
		t.Fatal("应报出块存储超额")
	}
	if !strings.Contains(it.Detail, "未含块卷") {
		t.Errorf("兜底时详情必须说明不含块卷，实际 %q", it.Detail)
	}
	if it.Severity != RiskAttention {
		t.Errorf("严重度 = %q，期望 attention", it.Severity)
	}
}

// NLB 不在任何免费额度里，只要存在就在计费。
func TestAssessBillingRiskNLB(t *testing.T) {
	in := riskInput()
	in.NLBCount = 1

	risk := AssessBillingRisk(in)
	it, ok := riskKeys(risk)[RiskNLB]
	if !ok {
		t.Fatal("有 NLB 时应报出")
	}
	if it.Severity != RiskBilling {
		t.Errorf("严重度 = %q，期望 billing", it.Severity)
	}
	// $0.0225/h × 24 × 7.2 ≈ 3.89
	if it.EstDailyCNY < 3.8 || it.EstDailyCNY > 4.0 {
		t.Errorf("估算日费用 = %v，期望约 3.89", it.EstDailyCNY)
	}
	// 流量处理费没算进去，必须说清楚，否则用户会拿这个数字去对账单。
	if !strings.Contains(it.Basis, "未包含") {
		t.Errorf("应说明流量费未计入，实际 %q", it.Basis)
	}

	in.NLBCount = 0
	if _, ok := riskKeys(AssessBillingRisk(in))[RiskNLB]; ok {
		t.Error("没有 NLB 时不该报")
	}
}

func TestAssessBillingRiskReservedIP(t *testing.T) {
	in := riskInput()
	in.ReservedPublicIPs = 2

	it, ok := riskKeys(AssessBillingRisk(in))[RiskReservedPublicIP]
	if !ok {
		t.Fatal("有保留 IP 时应报出")
	}
	// 2 个 × $0.0025 × 24 × 7.2 ≈ 0.86
	if it.EstDailyCNY < 0.8 || it.EstDailyCNY > 0.9 {
		t.Errorf("估算日费用 = %v，期望约 0.86", it.EstDailyCNY)
	}
}

// 出站流量只在超过免费额度时才报，且只算超出部分。
func TestAssessBillingRiskEgress(t *testing.T) {
	in := riskInput()
	// 8 TB，还在 10 TB 免费额度内。
	in.EgressGB = 8 * 1024
	if _, ok := riskKeys(AssessBillingRisk(in))[RiskCrossRegionEgress]; ok {
		t.Error("未超额时不该报")
	}

	// 12 TB，超出 2 TB。
	in.EgressGB = 12 * 1024
	it, ok := riskKeys(AssessBillingRisk(in))[RiskCrossRegionEgress]
	if !ok {
		t.Fatal("超额时应报出")
	}
	if !strings.Contains(it.Detail, "超出") {
		t.Errorf("详情应写明超出量，实际 %q", it.Detail)
	}
	// 2048 GB × $0.0085 × 7.2 / 30 ≈ 4.18
	if it.EstDailyCNY < 4.0 || it.EstDailyCNY > 4.4 {
		t.Errorf("估算日费用 = %v，期望约 4.18", it.EstDailyCNY)
	}
	// 额度每月重置，折算到日会误导，必须说明。
	if !strings.Contains(it.Basis, "重置") {
		t.Errorf("应说明额度每月重置，实际 %q", it.Basis)
	}

	// 查不到用量时不报——不知道不等于没风险，但也不该凭空报警。
	in.EgressKnown = false
	if _, ok := riskKeys(AssessBillingRisk(in))[RiskCrossRegionEgress]; ok {
		t.Error("查不到用量时不该报")
	}
}

// 多条规则同时命中时，总额要相加，月额按 30 天折算。
func TestAssessBillingRiskTotals(t *testing.T) {
	in := riskInput()
	in.ARM = LimitSnapshot{Used: 4, Limit: 2, Known: true}
	in.NLBCount = 1
	in.ReservedPublicIPs = 1

	risk := AssessBillingRisk(in)
	if len(risk.Items) != 3 {
		t.Fatalf("应有 3 条风险，实际 %d：%+v", len(risk.Items), risk.Items)
	}
	var sum float64
	for _, it := range risk.Items {
		sum += it.EstDailyCNY
	}
	if risk.TotalDailyCNY < sum-0.02 || risk.TotalDailyCNY > sum+0.02 {
		t.Errorf("总额 = %v，各项之和 = %v", risk.TotalDailyCNY, sum)
	}
	if risk.TotalMonthlyCNY < risk.TotalDailyCNY*29 {
		t.Errorf("月额 = %v，应约等于日额 × 30 = %v",
			risk.TotalMonthlyCNY, risk.TotalDailyCNY*30)
	}
	if !risk.HasBilling() {
		t.Error("应判为在计费")
	}
}

// 取数失败的项要把原因带出去，不能静默当成"没有风险"。
//
// 界面上「没查到」和「查了没问题」必须能分开——前者可能藏着真问题。
// 两种情形分开断言：
//   - 本地兜底也看得出超额 → 条目照常报出，但降级为 attention、不给金额
//   - 本地兜底看不出超额   → 仍然要出一条「没查到」，不能一声不响
func TestAssessBillingRiskCarriesErrors(t *testing.T) {
	t.Run("兜底也超额", func(t *testing.T) {
		in := riskInput()
		in.ARM = LimitSnapshot{Known: false}
		in.Instances = []store.Instance{{Shape: "VM.Standard.A1.Flex", Ocpus: 6}}
		in.Errors[RiskARMOver] = "NotAuthorized · 缺少 read limits 权限"

		it, ok := riskKeys(AssessBillingRisk(in))[RiskARMOver]
		if !ok {
			t.Fatal("应报出")
		}
		if !strings.Contains(it.Basis, "公开常量") {
			t.Errorf("应说明额度来自公开常量，实际 %q", it.Basis)
		}
		if it.Confirmed {
			t.Error("没读到 limits 时不能标为已确认")
		}
		if it.Severity != RiskAttention {
			t.Errorf("严重度 = %q，未确认的额度只能是 attention", it.Severity)
		}
		if it.EstDailyCNY != 0 {
			t.Errorf("没读到真实上限时不该给金额，实际 %v", it.EstDailyCNY)
		}
	})

	t.Run("兜底看不出超额", func(t *testing.T) {
		in := riskInput()
		in.ARM = LimitSnapshot{Known: false}
		in.Instances = []store.Instance{{Shape: "VM.Standard.A1.Flex", Ocpus: 2}}
		in.Errors[RiskARMOver] = "NotAuthorized · 缺少 read limits 权限"

		risk := AssessBillingRisk(in)
		it, ok := riskKeys(risk)[RiskARMOver]
		if !ok {
			t.Fatal("查不到时也要出一条，不能静默当成没风险")
		}
		if it.Severity != RiskAttention {
			t.Errorf("严重度 = %q，期望 attention", it.Severity)
		}
		if !strings.Contains(it.Detail, "缺少 read limits 权限") {
			t.Errorf("应带上失败原因，实际 %q", it.Detail)
		}
		if risk.HasBilling() {
			t.Error("没查到不等于在计费，不该触发计费告警")
		}
	})
}

// 所有规则的 key 必须稳定：前端按它匹配图标与文案。
func TestBillingRiskKeysStable(t *testing.T) {
	got := []string{
		RiskARMOver, RiskMicroOver, RiskBlockOver,
		RiskNLB, RiskReservedPublicIP, RiskCrossRegionEgress,
	}
	want := []string{
		"arm_over", "micro_over", "block_over",
		"nlb", "reserved_public_ip", "cross_region_egress",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 key = %q，期望 %q", i, got[i], want[i])
		}
	}
}

func TestRound2(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{1.234, 1.23}, {1.235, 1.24}, {0, 0}, {3.456, 3.46},
	}
	for _, c := range cases {
		if got := round2(c.in); got != c.want {
			t.Errorf("round2(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}
