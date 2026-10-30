package accountsvc

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// 计费风险规则的 key。前端按它匹配图标，改了就是静默失配。
const (
	RiskARMOver           = "arm_over"
	RiskMicroOver         = "micro_over"
	RiskBlockOver         = "block_over"
	RiskNLB               = "nlb"
	RiskReservedPublicIP  = "reserved_public_ip"
	RiskCrossRegionEgress = "cross_region_egress"
)

// 计费风险的严重度。
//
// 只有两级，且不是"严重/不严重"而是"已经在扣钱/可能会扣钱"：
//   - billing   已经确定在产生费用（超配额、有 NLB、有保留 IP、流量超标）
//   - attention 需要看一眼，但可能仍然免费（例如拿不到真实上限，只能按
//     公开额度推断）
const (
	RiskBilling   = "billing"
	RiskAttention = "attention"
)

// 单价常量，单位是美元。
//
// **这些数字是粗算用的，不是账单。** OCI 的标价随区域、币种、合同变化，
// 本工具不去拉价目表（那需要另一套权限，且会引入一张必然过期的表）。
// 所有用到它们的地方都必须在界面上标注「估算」。
//
// 核对日期：2026-08-18，来源为 Oracle 公开价目表的常见商业区域标价。
const (
	// priceA1OCPUHour 是 Ampere A1 每个 OCPU 每小时的单价。
	priceA1OCPUHour = 0.01
	// priceA1GBHour 是 Ampere A1 每 GB 内存每小时的单价。
	priceA1GBHour = 0.0015
	// priceMicroHour 是 E2.1.Micro 每台每小时的单价。
	priceMicroHour = 0.01
	// priceBlockGBMonth 是块存储每 GB 每月的单价。
	priceBlockGBMonth = 0.0255
	// priceReservedIPHour 是保留（未绑定）公网 IP 每小时的单价。
	priceReservedIPHour = 0.0025
	// priceNLBHour 是网络负载均衡的基础小时费（不含流量处理费）。
	priceNLBHour = 0.0225
	// priceEgressGB 是超出每月免费额度后每 GB 出站流量的单价。
	priceEgressGB = 0.0085
)

// usdToCNY 是估算用的汇率。
//
// 硬编码是因为拉实时汇率要引入一个外部依赖，而这里的输出本来就标着
// 「估算」。看到这个数字与账单对不上时，先怀疑它。
const usdToCNY = 7.2

// hoursPerDay 与 daysPerMonth 让日/月两种计价单位能互相换算。
const (
	hoursPerDay  = 24
	daysPerMonth = 30
)

// LimitSnapshot 是某一项限额的真实数值。
//
// Known 区分两种来源：
//   - true  来自账号自己的 limits 接口。这是**唯一可信**的数字。
//   - false 拿不到，退回到 ociclient/freetier.go 的公开常量。
//
// 这个区分是这张任务卡最重要的一条纪律：Oracle 会不打招呼地改免费额度
// （2026-06 把 A1 从 4/24 砍到 2/12），所以常量只能用来提示，不能用来
// 断言"你超额了"。
type LimitSnapshot struct {
	Used  int64
	Limit int64
	Known bool
}

// RiskInput 是计算所需的全部输入。
//
// 由调用方（httpapi）把各处取好的数据组装进来，本包只做纯计算。
// 这样「六条规则」可以在表驱动测试里穷举，不必搭一整套假 OCI。
type RiskInput struct {
	Account store.Account
	// Instances 是该账号在面板里同步到的实例（不含已终止）。
	Instances []store.Instance

	// ARM 与 Micro 的用量与上限。Known 为 false 时按公开常量兜底。
	ARM   LimitSnapshot
	Micro LimitSnapshot
	Block LimitSnapshot

	// ReservedPublicIPs 是保留型公网 IP 的数量（RESERVED，不是临时 IP）。
	ReservedPublicIPs int
	NLBCount          int

	// EgressGB 是本月至今的出站流量；EgressKnown 为 false 表示没查到。
	EgressGB    float64
	EgressKnown bool
	// FreeEgressGB 是免费额度。来自 billingsvc.FreeEgressGB，
	// 由调用方传进来——本包不依赖 billingsvc，避免两个 service 互相牵连。
	FreeEgressGB float64

	// Errors 记录各组取数失败的原因，键是规则的 key。
	// 取不到数据不等于没有风险，界面上要说清楚"这项没查到"。
	Errors map[string]string
}

// BillingRiskItem 是一条计费风险。
type BillingRiskItem struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Severity string `json:"severity"`
	// Detail 是具体数字，例如「ARM 3 OCPU / 16 GB，免费额度 2 / 12」。
	Detail string `json:"detail"`
	// EstDailyCNY 是估算的每日费用。0 表示估算不出或确实免费。
	EstDailyCNY float64 `json:"estDailyCny"`
	// Basis 是估算依据，必须原样显示——「估算」两个字不能只写在标题里。
	Basis string `json:"basis"`
	// Confirmed 表示额度数字来自账号自己的 limits 接口。
	// false 时界面必须写成「按公开免费额度估算」而不是「你超额了」。
	Confirmed bool `json:"confirmed"`
	// Notice 是取数失败等额外说明。
	Notice string `json:"notice,omitempty"`
}

// riskRuleLabels 是规则的显示名，用于「这项没查到」时的条目。
var riskRuleLabels = map[string]string{
	RiskARMOver:           "ARM 免费额度",
	RiskMicroOver:         "Micro 免费额度",
	RiskBlockOver:         "块存储免费额度",
	RiskNLB:               "网络负载均衡",
	RiskReservedPublicIP:  "保留公网 IP",
	RiskCrossRegionEgress: "出站流量",
}

// riskRuleOrder 固定输出顺序，让同一账号两次请求的条目顺序一致。
var riskRuleOrder = []string{
	RiskARMOver, RiskMicroOver, RiskBlockOver,
	RiskNLB, RiskReservedPublicIP, RiskCrossRegionEgress,
}

// BillingRisk 是一个账号的计费风险评估。
type BillingRisk struct {
	AccountID   string            `json:"accountId"`
	Region      string            `json:"region"`
	GeneratedAt time.Time         `json:"generatedAt"`
	Items       []BillingRiskItem `json:"items"`
	// TotalDailyCNY 是各项估算日费用之和。
	TotalDailyCNY float64 `json:"totalDailyCny"`
	// TotalMonthlyCNY 是按 30 天折算的月费用。
	TotalMonthlyCNY float64 `json:"totalMonthlyCny"`
	// Notice 是整体说明，必须包含「估算」二字。
	Notice string `json:"notice"`
}

// HasBilling 表示至少有一项已经确定在产生费用。
func (r BillingRisk) HasBilling() bool {
	for _, it := range r.Items {
		if it.Severity == RiskBilling {
			return true
		}
	}
	return false
}

// AssessBillingRisk 按六条规则算出计费风险。
//
// 这是纯函数：不调 API、不看时钟以外的东西，所以每条规则都能单独断言。
func AssessBillingRisk(in RiskInput) BillingRisk {
	risk := BillingRisk{
		AccountID:   in.Account.ID,
		Region:      in.Account.DefaultRegion,
		GeneratedAt: time.Now(),
		Notice: "费用为按公开标价粗算的估算值，仅用于提示；实际金额以 Oracle 账单为准。" +
			"免费额度以账号自己的 limits 接口为准，公开额度可能已经过时。",
	}

	arm := in.ARM
	if !arm.Known {
		arm = localARMSnapshot(in.Instances)
	}
	if it, ok := assessARM(arm); ok {
		risk.Items = append(risk.Items, it)
	}

	micro := in.Micro
	if !micro.Known {
		micro = localMicroSnapshot(in.Instances)
	}
	if it, ok := assessMicro(micro); ok {
		risk.Items = append(risk.Items, it)
	}

	block := in.Block
	if !block.Known {
		block = localBlockSnapshot(in.Instances)
	}
	if it, ok := assessBlock(block); ok {
		risk.Items = append(risk.Items, it)
	}

	if it, ok := assessNLB(in.NLBCount, in.Errors[RiskNLB]); ok {
		risk.Items = append(risk.Items, it)
	}
	if it, ok := assessReservedIP(in.ReservedPublicIPs, in.Errors[RiskReservedPublicIP]); ok {
		risk.Items = append(risk.Items, it)
	}
	if it, ok := assessEgress(in.EgressGB, in.EgressKnown, in.FreeEgressGB,
		in.Errors[RiskCrossRegionEgress]); ok {
		risk.Items = append(risk.Items, it)
	}

	// 取不到数据的规则也必须出声。
	//
	// 上面六条规则只在「确实有风险」时产出条目，于是取数失败时它们一言不发，
	// 整个响应就是 items: [] —— 界面显示「一切正常」，而实际上是什么都没查到。
	// 这两件事必须分开：看到「没风险」用户会安心走开，看到「这项没查到」
	// 才会去补权限。凭据解不开时更是六项全空，那时说「正常」是纯粹的误导。
	emitted := make(map[string]bool, len(risk.Items))
	for _, it := range risk.Items {
		emitted[it.Key] = true
	}
	for _, key := range riskRuleOrder {
		msg := strings.TrimSpace(in.Errors[key])
		if msg == "" || emitted[key] {
			continue
		}
		risk.Items = append(risk.Items, BillingRiskItem{
			Key:      key,
			Label:    riskRuleLabels[key],
			Severity: RiskAttention,
			Detail:   "这项没能取到数据，因此无法确认是否有费用：" + msg,
			// 不给金额：数字站不住脚时就不要给。
			Basis:     "取数失败时既不断言有费用，也不当成没有风险",
			Confirmed: false,
			Notice:    msg,
		})
	}

	for _, it := range risk.Items {
		risk.TotalDailyCNY += it.EstDailyCNY
	}
	risk.TotalDailyCNY = round2(risk.TotalDailyCNY)
	risk.TotalMonthlyCNY = round2(risk.TotalDailyCNY * daysPerMonth)

	// 按固定顺序输出。上面分两批 append，不打散的话「未查到」永远排在
	// 「在扣钱」后面，读起来像是两个不相干的列表。
	sort.SliceStable(risk.Items, func(i, j int) bool {
		return riskRuleIndex(risk.Items[i].Key) < riskRuleIndex(risk.Items[j].Key)
	})
	return risk
}

// riskRuleIndex 返回规则在固定顺序里的位置，未知 key 排最后。
func riskRuleIndex(key string) int {
	for i, k := range riskRuleOrder {
		if k == key {
			return i
		}
	}
	return len(riskRuleOrder)
}

// ---- 六条规则 ----

// assessARM 判断 ARM 用量是否超出免费额度。
//
// 拆成 OCPU 与内存两条算式但合成一项输出：用户看到的是「ARM 超额」，
// 而不是两条需要自己相加的账。
func assessARM(s LimitSnapshot) (BillingRiskItem, bool) {
	// 只判 OCPU，不判内存。
	//
	// limits 接口对 A1 只给 OCPU 条数（standard-a1-core-regional-count），
	// 内存跟着 OCPU 的配比走，没有独立的一条。按 1:6 折算看着可行，
	// 但用户完全可能开一个 1 OCPU / 24 GB 的自定义形状——那时折算出来的
	// 内存上限就是错的，会把一个真实超额报成"没超"。
	// 宁可少报一项，也不报错的。
	if s.Used <= s.Limit {
		return BillingRiskItem{}, false
	}

	over := s.Used - s.Limit
	daily := float64(over) * priceA1OCPUHour * hoursPerDay * usdToCNY
	// 内存按同样比例超出的假设偏保守：实际可能只超 OCPU 不超内存。
	// 这里只算 OCPU，把内存作为提示写在 Detail 里，不去猜。
	item := BillingRiskItem{
		Key:         RiskARMOver,
		Label:       "ARM 超出免费额度",
		Severity:    RiskBilling,
		Detail:      fmt.Sprintf("ARM 已用 %d OCPU，免费额度 %d OCPU，超出 %d 个", s.Used, s.Limit, over),
		EstDailyCNY: round2(daily),
		Basis: fmt.Sprintf("按 $%.4f / OCPU·小时 × %d 小时 × 汇率 %.1f 粗算；内存未计入",
			priceA1OCPUHour, hoursPerDay, usdToCNY),
		Confirmed: s.Known,
	}
	if !s.Known {
		item.Detail += "（额度按公开免费额度估算）"
		item.Severity = RiskAttention
		item.EstDailyCNY = 0
		item.Basis = fmt.Sprintf("免费额度 %d OCPU 来自 ociclient/freetier.go 的公开常量，"+
			"拿不到账号的 limits 时只能作提示，不能据此断言超额", ociclient.AlwaysFreeARMOcpus)
	}
	return item, true
}

func assessMicro(s LimitSnapshot) (BillingRiskItem, bool) {
	if s.Used <= s.Limit {
		return BillingRiskItem{}, false
	}
	over := s.Used - s.Limit
	daily := float64(over) * priceMicroHour * hoursPerDay * usdToCNY
	item := BillingRiskItem{
		Key:         RiskMicroOver,
		Label:       "Micro 实例超出免费额度",
		Severity:    RiskBilling,
		Detail:      fmt.Sprintf("E2.1.Micro 已开 %d 台，免费额度 %d 台，超出 %d 台", s.Used, s.Limit, over),
		EstDailyCNY: round2(daily),
		Basis:       fmt.Sprintf("按 $%.4f / 台·小时 × %d 小时 × 汇率 %.1f 粗算", priceMicroHour, hoursPerDay, usdToCNY),
		Confirmed:   s.Known,
	}
	if !s.Known {
		item.Detail += "（额度按公开免费额度估算）"
		item.Severity = RiskAttention
		item.EstDailyCNY = 0
		item.Basis = fmt.Sprintf("免费额度 %d 台来自公开常量，拿不到 limits 时只作提示",
			ociclient.AlwaysFreeMicroInstances)
	}
	return item, true
}

func assessBlock(s LimitSnapshot) (BillingRiskItem, bool) {
	if s.Used <= s.Limit {
		return BillingRiskItem{}, false
	}
	over := s.Used - s.Limit
	daily := float64(over) * priceBlockGBMonth / daysPerMonth * usdToCNY
	item := BillingRiskItem{
		Key:         RiskBlockOver,
		Label:       "块存储超出免费额度",
		Severity:    RiskBilling,
		Detail:      fmt.Sprintf("块存储已用 %d GB，免费额度 %d GB，超出 %d GB", s.Used, s.Limit, over),
		EstDailyCNY: round2(daily),
		Basis: fmt.Sprintf("按 $%.4f / GB·月 ÷ %d 天 × 汇率 %.1f 粗算",
			priceBlockGBMonth, daysPerMonth, usdToCNY),
		Confirmed: s.Known,
	}
	if !s.Known {
		item.Detail += "（额度按公开免费额度估算，且未含块卷）"
		item.Severity = RiskAttention
		item.EstDailyCNY = 0
		item.Basis = fmt.Sprintf("免费额度 %d GB 来自公开常量；本地只有引导卷容量，"+
			"块卷要查 limits 才知道", ociclient.AlwaysFreeBlockGB)
	}
	return item, true
}

// assessNLB 判断是否存在网络负载均衡。
//
// NLB 不在任何免费额度里，只要存在就在计费。所以这里不比较数量，
// 有就报。
func assessNLB(count int, errText string) (BillingRiskItem, bool) {
	if count <= 0 {
		return BillingRiskItem{}, false
	}
	daily := float64(count) * priceNLBHour * hoursPerDay * usdToCNY
	return BillingRiskItem{
		Key:         RiskNLB,
		Label:       "存在网络负载均衡",
		Severity:    RiskBilling,
		Detail:      fmt.Sprintf("%d 个网络负载均衡（NLB 不在免费额度内）", count),
		EstDailyCNY: round2(daily),
		Basis: fmt.Sprintf("只算基础小时费 $%.4f / 小时 × %d 小时 × 汇率 %.1f；"+
			"流量处理费另计，未包含", priceNLBHour, hoursPerDay, usdToCNY),
		Confirmed: true,
		Notice:    errText,
	}, true
}

// assessReservedIP 判断是否存在保留型公网 IP。
//
// 临时（EPHEMERAL）公网 IP 挂在运行中的实例上不额外收费；
// 保留（RESERVED）但没绑定的会按小时计费。判定在取数侧完成，
// 这里只看数量。
func assessReservedIP(count int, errText string) (BillingRiskItem, bool) {
	if count <= 0 {
		return BillingRiskItem{}, false
	}
	daily := float64(count) * priceReservedIPHour * hoursPerDay * usdToCNY
	return BillingRiskItem{
		Key:         RiskReservedPublicIP,
		Label:       "存在保留公网 IP",
		Severity:    RiskBilling,
		Detail:      fmt.Sprintf("%d 个保留公网 IP（未绑定实例时会按小时计费）", count),
		EstDailyCNY: round2(daily),
		Basis: fmt.Sprintf("按 $%.4f / 个·小时 × %d 小时 × 汇率 %.1f 粗算",
			priceReservedIPHour, hoursPerDay, usdToCNY),
		Confirmed: true,
		Notice:    errText,
	}, true
}

// assessEgress 判断本月出站流量是否超出免费额度。
//
// 免费额度是每月 10 TB，按自然月重置。这里只报超出部分——
// 用量没超时不输出任何东西，因为"用了 8 TB 但还在免费额度内"不是风险。
func assessEgress(usedGB float64, known bool, freeGB float64, errText string) (BillingRiskItem, bool) {
	if !known || freeGB <= 0 || usedGB <= freeGB {
		return BillingRiskItem{}, false
	}
	over := usedGB - freeGB
	daily := over * priceEgressGB * usdToCNY / daysPerMonth
	return BillingRiskItem{
		Key:      RiskCrossRegionEgress,
		Label:    "出站流量超出免费额度",
		Severity: RiskBilling,
		Detail: fmt.Sprintf("本月出站 %.1f GB，免费额度 %.0f GB，超出 %.1f GB",
			usedGB, freeGB, over),
		EstDailyCNY: round2(daily),
		Basis: fmt.Sprintf("超出部分按 $%.4f / GB × 汇率 %.1f 计，按月折算到日；"+
			"额度每月重置，不是每天都超", priceEgressGB, usdToCNY),
		Confirmed: true,
		Notice:    errText,
	}, true
}

// ---- 本地兜底：拿不到 limits 时从面板自己的实例表推算 ----

// localARMSnapshot 从实例表汇总 ARM 用量。
//
// 上限用公开常量：调用方已经确认 limits 拿不到，此时能用的只有它。
// Known 保持 false，让上层文案标明这是估算。
func localARMSnapshot(instances []store.Instance) LimitSnapshot {
	var used int64
	for _, i := range instances {
		if !strings.HasPrefix(i.Shape, "VM.Standard.A1") {
			continue
		}
		used += int64(i.Ocpus)
	}
	return LimitSnapshot{Used: used, Limit: ociclient.AlwaysFreeARMOcpus, Known: false}
}

func localMicroSnapshot(instances []store.Instance) LimitSnapshot {
	var used int64
	for _, i := range instances {
		if i.Shape == "VM.Standard.E2.1.Micro" {
			used++
		}
	}
	return LimitSnapshot{Used: used, Limit: ociclient.AlwaysFreeMicroInstances, Known: false}
}

// localBlockSnapshot 用引导卷容量兜底。
//
// 一定偏低：块卷（block volume）不在实例表里。所以 Known=false 时的文案
// 必须说明「未含块卷」，否则用户会以为 150 GB 就是全部。
func localBlockSnapshot(instances []store.Instance) LimitSnapshot {
	var used int64
	for _, i := range instances {
		used += i.BootVolumeGB
	}
	return LimitSnapshot{Used: used, Limit: ociclient.AlwaysFreeBlockGB, Known: false}
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
