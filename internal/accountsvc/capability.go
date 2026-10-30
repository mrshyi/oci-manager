package accountsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// 能力探测的五个族。
//
// 选这五个而不是把 OCI 全部服务都试一遍：它们正好对应本工具真正会去调的五类
// 接口，而且各自需要不同的 IAM 策略。缺了哪一族，界面上就能直接说清楚
// 「这个账号用不了哪个功能」，而不是等用户点进去才收到一个 403。
const (
	CapCompute    = "compute"    // ListInstances
	CapNetwork    = "network"    // ListVcns
	CapStorage    = "storage"    // ListVolumes
	CapUsage      = "usage"      // RequestSummarizedUsages
	CapMonitoring = "monitoring" // SummarizeMetrics（即 ListMetrics）
)

// capabilitySpec 是一族探测的静态描述。
type capabilitySpec struct {
	Key   string
	Label string
	// Policy 是这族能力需要的 IAM 策略。缺权限时直接告诉用户该加哪一条，
	// 比让他去翻文档强得多。
	Policy string
}

// capabilitySpecs 的顺序就是界面上的展示顺序。
//
// Policy 一栏必须与 policytpl.go 里那 8 条策略对得上——两处都提到策略，
// 说的是同一件事却给出不同写法，用户会不知道该信哪个。有测试钉住
// （TestCapabilityPoliciesMatchTemplate）。
//
// compute / network / storage 排前面：它们是本工具的主干功能，缺了基本没法用。
// usage / monitoring 是锦上添花，缺了只影响账单页与图表。
var capabilitySpecs = []capabilitySpec{
	{CapCompute, "实例", "read instances in tenancy"},
	{CapNetwork, "网络", "read virtual-network-family in tenancy"},
	{CapStorage, "存储", "read volume-family in tenancy"},
	{CapUsage, "用量账单", "read usage-report in tenancy"},
	{CapMonitoring, "监控指标", "read metrics in tenancy"},
}

// CapabilitySpec 是下发给前端的族描述。
type CapabilitySpec struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Policy string `json:"policy"`
}

// CapabilitySpecs 返回五族的静态描述。
//
// 由后端下发而不是前端硬编码：后端加了第六族，前端不跟着改就会少显示一项，
// 而且不会有任何报错——这种静默缺失最难发现。
func CapabilitySpecs() []CapabilitySpec {
	out := make([]CapabilitySpec, 0, len(capabilitySpecs))
	for _, s := range capabilitySpecs {
		out = append(out, CapabilitySpec{Key: s.Key, Label: s.Label, Policy: s.Policy})
	}
	return out
}

// Capability 是一族能力的探测结论。
type Capability struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// OK 表示这族能力可用：接口调通了，拿到了数据。
	OK bool `json:"ok"`
	// Denied 表示不可用的原因是缺权限。与"接口报错"必须分开——
	// 前者要去加策略，后者过一会儿重试可能就好了。
	Denied bool `json:"denied"`
	// Class 是失败分类名（NotAuthorized / Throttled / BadRequest …）。
	Class string `json:"class,omitempty"`
	// Detail 面向用户的一句话说明。
	Detail string `json:"detail,omitempty"`
	// Policy 是缺权限时该补的 IAM 策略。
	Policy string `json:"policy,omitempty"`
	// Count 是探测到的资源数量，只有 compute / network / storage 有意义。
	Count int `json:"count"`
	// DurationMs 这一族花了多久。慢的那一族往往就是问题所在。
	DurationMs int64 `json:"durationMs"`
}

// CapabilityProbe 是一次完整探测的结果，会被序列化成 JSON 存进
// accounts.capability_probe。
type CapabilityProbe struct {
	AccountID string    `json:"accountId"`
	Region    string    `json:"region"`
	ProbedAt  time.Time `json:"probedAt"`
	Items     []Capability `json:"items"`
	// Summary 一句话结论，列表页直接显示。
	Summary string `json:"summary"`
}

// OKCount 返回可用的族数。
func (p CapabilityProbe) OKCount() int {
	n := 0
	for _, c := range p.Items {
		if c.OK {
			n++
		}
	}
	return n
}

// DeniedKeys 返回缺权限的族名，用于拼提示文案。
func (p CapabilityProbe) DeniedKeys() []string {
	out := make([]string, 0, len(p.Items))
	for _, c := range p.Items {
		if c.Denied {
			out = append(out, c.Label)
		}
	}
	return out
}

// FindCapability 按 key 取一项，找不到返回零值。
func (p CapabilityProbe) FindCapability(key string) (Capability, bool) {
	for _, c := range p.Items {
		if c.Key == key {
			return c, true
		}
	}
	return Capability{}, false
}

// capabilityTimeout 是单族探测的上限。
//
// 比连通性校验的 20 秒宽松一些：这五个是真正在拉数据（可能几百台实例），
// 而连通性校验只是取一个用户对象。但也不能太长——用户在等结果。
const capabilityTimeout = 25 * time.Second

// CapabilityOptions 是探测所需的上下文。
type CapabilityOptions struct {
	// Region 与 CompartmentID 留空时用账号自己的默认值。
	Region        string
	CompartmentID string
	TenancyID     string
}

// ProbeCapabilities 对五族各打一次只读接口，判断这个账号能干什么。
//
// 全部请求都带 WithoutBudget：探测不是业务动作，不该占用用户的调用额度。
//
// 顺序上有一个刻意的取舍：先单独跑 compute，只有它没撞上签名类失败时才并发
// 跑其余四个。凭据坏掉时（AuthFailed）剩下四个的结论必然一样，连打四次只会
// 让账号更快被限流——而校验按钮是用户能反复点的，失败路径上的请求量要尽量小。
func ProbeCapabilities(ctx context.Context, client *ociclient.Client, opts CapabilityOptions) CapabilityProbe {
	probe := CapabilityProbe{Region: opts.Region, ProbedAt: time.Now()}

	first := runCapability(ctx, client, capabilitySpecs[0], opts)
	probe.Items = append(probe.Items, first)

	rest := capabilitySpecs[1:]
	if first.Class == ociclient.ClassAuthFailed.String() {
		// 签名本身没过，后面四个必然同样失败。不再发请求，直接复制结论
		// 并标注"未探测"——不能让它看起来像是探测过且真的缺权限。
		for _, spec := range rest {
			probe.Items = append(probe.Items, Capability{
				Key: spec.Key, Label: spec.Label, Policy: spec.Policy,
				Class:  first.Class,
				Detail: "未探测：凭据校验失败，同一份签名重试只会得到相同结果",
			})
		}
		probe.Summary = summarizeProbe(probe)
		return probe
	}

	// 其余四族互不依赖，并发发出去。
	results := make([]Capability, len(rest))
	var wg sync.WaitGroup
	for i, spec := range rest {
		wg.Add(1)
		go func(i int, spec capabilitySpec) {
			defer wg.Done()
			results[i] = runCapability(ctx, client, spec, opts)
		}(i, spec)
	}
	wg.Wait()
	probe.Items = append(probe.Items, results...)

	probe.Summary = summarizeProbe(probe)
	return probe
}

// runCapability 执行单族探测。
func runCapability(ctx context.Context, client *ociclient.Client,
	spec capabilitySpec, opts CapabilityOptions) Capability {

	c := Capability{Key: spec.Key, Label: spec.Label, Policy: spec.Policy}
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, capabilityTimeout)
	defer cancel()
	ctx = ociclient.WithoutBudget(ctx)

	count, err := callCapability(ctx, client, spec.Key, opts)
	c.DurationMs = time.Since(start).Milliseconds()
	c.Count = count

	if err == nil {
		c.OK = true
		return c
	}

	if apiErr, ok := ociclient.AsAPIError(err); ok {
		c.Class = apiErr.Class.String()
		// AuthFailed 与 NotAuthorized 都归为"缺权限/凭据不可用"。
		// OCI 对这两者共用 NotAuthorizedOrNotFound，区分不出来，
		// 所以文案上也不区分——统一说"没有权限访问"，并给出该补的策略。
		c.Denied = apiErr.Class == ociclient.ClassNotAuthorized ||
			apiErr.Class == ociclient.ClassAuthFailed
		c.Detail = shortError(err)
		return c
	}
	// 非 API 错误（DNS、TLS、超时）没有分类。不是权限问题，但确实不可用。
	c.Detail = err.Error()
	return c
}

// callCapability 按族名调用对应的只读接口，返回探测到的资源数量。
func callCapability(ctx context.Context, client *ociclient.Client, key string,
	opts CapabilityOptions) (int, error) {

	region := opts.Region
	if region == "" {
		region = client.Region()
	}
	compartment := opts.CompartmentID
	if compartment == "" {
		compartment = opts.TenancyID
	}

	switch key {
	case CapCompute:
		// Limit 1 就够：这里问的是"能不能列"，不是"有多少"。
		// 但 OCI 的 limit 是单页条数，仍会翻页——所以取 len 得到的是真实总数，
		// 对一个只做权限自检的调用来说有点浪费。
		// 用一个足够小的 limit 换取"不翻页"：Limit=1 时若还有 nextPage，
		// ListInstances 仍会继续翻。这是既有实现的行为，不去动它。
		list, err := client.ListInstances(ctx, ociclient.ListInstancesOptions{
			CompartmentID: compartment,
			Region:        region,
			Limit:         100,
		})
		return len(list), err

	case CapNetwork:
		list, err := client.ListVcns(ctx, region, compartment)
		return len(list), err

	case CapStorage:
		list, err := client.ListVolumes(ctx, region, compartment)
		return len(list), err

	case CapUsage:
		// Usage API 的数据是租户全局的，必须用租户根 OCID，不能用子分区。
		// 区间取最近一个完整自然日：DAILY 粒度要求两端对齐 UTC 零点。
		end := ociclient.AlignDay(time.Now())
		start := end.AddDate(0, 0, -1)
		items, err := client.SummarizeUsage(ctx, ociclient.UsageQuery{
			TenantID:    opts.TenancyID,
			Start:       start,
			End:         end,
			Granularity: ociclient.GranularityDaily,
			QueryType:   ociclient.QueryTypeCost,
			Region:      region,
		})
		return len(items), err

	case CapMonitoring:
		// 用一条不带资源维度的聚合查询：这里要验证的是"能读监控数据"，
		// 不是"能读某台机器的数据"。空结果也算成功——一个没有实例的
		// 分区本来就该返回空。
		now := time.Now()
		series, err := client.SummarizeMetrics(ctx, ociclient.MetricQuery{
			CompartmentID: compartment,
			Region:        region,
			Namespace:     "oci_computeagent",
			Query:         "CpuUtilization[1h].mean()",
			StartTime:     now.Add(-1 * time.Hour),
			EndTime:       now,
		})
		return len(series), err
	}
	return 0, fmt.Errorf("未知的能力族 %q", key)
}

// summarizeProbe 拼一句结论。
//
// 优先说缺什么，而不是"3/5 可用"——用户看到 3/5 的第一反应是"哪两个"，
// 直接把名字点出来省一次追问。
func summarizeProbe(p CapabilityProbe) string {
	total := len(p.Items)
	ok := p.OKCount()
	if ok == total {
		return fmt.Sprintf("%d 项能力全部可用", total)
	}
	if denied := p.DeniedKeys(); len(denied) > 0 {
		return fmt.Sprintf("%d/%d 项可用 · 缺少%s权限", ok, total, strings.Join(denied, "、"))
	}
	return fmt.Sprintf("%d/%d 项可用 · 其余接口调用异常", ok, total)
}

// ---- 落库 ----

// EncodeCapabilityProbe 把探测结果序列化成存库用的字符串。
//
// 序列化失败只可能是程序错误（结构里有 channel 之类），此时返回空串让调用方
// 当作"没有结果"处理，而不是把一个半截的 JSON 写进库。
func EncodeCapabilityProbe(p CapabilityProbe) string {
	if len(p.Items) == 0 {
		return ""
	}
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(data)
}

// DecodeCapabilityProbe 解析库里存的 JSON。空串或坏数据返回 nil。
//
// 不返回错误：库里的旧数据格式变了不该让整个列表接口 500。解析不出来
// 就当没探测过，用户点一次「自检」就能刷新。
func DecodeCapabilityProbe(raw string) *CapabilityProbe {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var p CapabilityProbe
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil
	}
	if len(p.Items) == 0 {
		return nil
	}
	return &p
}

// ProbeFromAccount 取出账号上次探测的结果，供列表页与详情页使用。
func ProbeFromAccount(acc store.Account) *CapabilityProbe {
	return DecodeCapabilityProbe(acc.CapabilityProbe)
}
