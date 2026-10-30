package accountsvc

import (
	"fmt"
	"strings"
)

// 云侧策略模板（P4）。
//
// 这一份东西的全部意义是「让用户把面板凭据的权限限制到刚好够用」。
// 所以它必须是**静态文案**，不是策略生成器：
//
//   - 生成策略需要面板持有 `manage policies in tenancy`——为了限制面板的权限
//     先给它一个更大的权限，逻辑上是拧的（方案 ADR-2 的结论）。
//   - 面板不建组、不写策略、不改 IAM。用户自己粘 8 条到控制台，
//     粘完点一次能力探测就知道配对了没有。
//
// 注意这里没有 manage policies、没有 manage users、没有 manage groups。
// 少了这三条，凭据泄露后的爆炸半径小得多。
const (
	// PolicyGroupPlaceholder 是组名的占位符。
	//
	// 用中文尖括号而不是 {{group}}：用户是把这段文本直接粘进 OCI 控制台的，
	// 占位符要一眼能看出"这里得改"，而花括号在策略语法里看起来像格式串。
	PolicyGroupPlaceholder = "<你的组>"

	// PolicyScope 是策略作用域。
	//
	// 用 tenancy 而不是 compartment：本工具管理的是「整个账号下有哪些实例」，
	// 而 compartment 级策略在新建 compartment 后会突然失效——那种失败
	// 表现为"某些资源莫名其妙看不到"，最难排查。
	PolicyScope = "in tenancy"
)

// PolicyLine 是一条策略。
type PolicyLine struct {
	// Text 是可直接粘贴的完整一行，占位符已替换。
	Text string `json:"text"`
	// Purpose 说明这条为什么需要。用户要能判断"我能不能不给这条"。
	Purpose string `json:"purpose"`
	// Capabilities 是这条策略覆盖的能力探测族。
	//
	// 它让「探测说缺某族权限」与「模板里该补哪条」能对上号。
	// 有测试钉住：每一族都必须至少被一条策略覆盖。
	Capabilities []string `json:"capabilities"`
}

// policyLines 是 8 条策略的定义。
//
// 顺序是刻意的：先读后写、按资源族成对出现。用户从上往下粘，
// 中途停下时手里是一份"能看不能改"的权限集——那是安全的方向。
var policyLines = []struct {
	verb     string
	family   string
	purpose  string
	caps     []string
	readOnly bool
}{
	{"read", "instances", "列出实例、查看详情与状态。没有它面板里一台机器都看不到", []string{CapCompute}, true},
	{"manage", "instances", "开机、关机、重启、改规格、终止、连控制台", []string{CapCompute}, false},
	{"read", "virtual-network-family", "列出 VCN、子网、安全列表与公网 IP", []string{CapNetwork}, true},
	{"manage", "virtual-network-family", "放行端口、换公网 IP、建 NAT 网关与负载均衡", []string{CapNetwork}, false},
	{"read", "volume-family", "列出引导卷与块存储，查看容量与 VPU", []string{CapStorage}, true},
	{"manage", "volume-family", "扩容引导卷、调整 VPU、分离与挂载", []string{CapStorage}, false},
	{"read", "usage-report", "读取本月用量与出站流量（账单页与计费风险都要它）", []string{CapUsage}, true},
	{"read", "metrics", "读取 CPU 与网络指标，画实例详情里的图表", []string{CapMonitoring}, true},
}

// PolicyTemplate 是下发给前端的完整模板。
type PolicyTemplate struct {
	// Group 是用户填的组名，未填时等于占位符。
	Group string `json:"group"`
	// Text 是 8 条拼好的全文，一键复制用。
	Text     string       `json:"text"`
	Lines    []PolicyLine `json:"lines"`
	Steps    []string     `json:"steps"`
	Scope    string       `json:"scope"`
	Notice   string       `json:"notice"`
	Version  string       `json:"version"`
	ReadOnly int          `json:"readOnlyCount"`
}

// PolicyTemplateVersion 是模板文案的版本。
//
// 文案改了版本号就加一，前端可以据此提示"你之前复制的是旧版"。
// 不引入日期：版本号只需要能比较新旧。
const PolicyTemplateVersion = "1"

// BuildPolicyTemplate 生成策略模板。group 为空时保留占位符。
func BuildPolicyTemplate(group string) PolicyTemplate {
	group = strings.TrimSpace(group)
	// 组名会进策略语句，必须挡住能改变语句结构的东西。
	// 这不是安全边界（策略最终由 OCI 校验），而是别让用户复制出一份
	// 语法错误的文本、然后以为是面板写错了。
	if group == "" || !validGroupName(group) {
		group = PolicyGroupPlaceholder
	}

	lines := make([]PolicyLine, 0, len(policyLines))
	texts := make([]string, 0, len(policyLines))
	readOnly := 0
	for _, pl := range policyLines {
		text := fmt.Sprintf("Allow group %s to %s %s %s", group, pl.verb, pl.family, PolicyScope)
		lines = append(lines, PolicyLine{
			Text:         text,
			Purpose:      pl.purpose,
			Capabilities: pl.caps,
		})
		texts = append(texts, text)
		if pl.readOnly {
			readOnly++
		}
	}

	return PolicyTemplate{
		Group:    group,
		Text:     strings.Join(texts, "\n"),
		Lines:    lines,
		Steps:    policyCreationSteps(group),
		Scope:    PolicyScope,
		ReadOnly: readOnly,
		Version:  PolicyTemplateVersion,
		Notice: "这 8 条里没有 manage policies、manage users、manage groups——" +
			"面板不建组、不写策略、不改 IAM。凭据泄露时能造成的破坏被限制在" +
			"「动资源」而不是「改权限」。粘完之后回到账号页点一次「权限自检」，" +
			"五个点全绿就说明配对了。",
	}
}

// policyCreationSteps 是「怎么建组和用户」的四步说明。
//
// 写具体菜单路径而不是泛泛而谈：用户在这一步已经因为 403 而烦躁了，
// 让他去翻文档找「Identity 在哪」等于把问题推回去。
func policyCreationSteps(group string) []string {
	return []string{
		"在 OCI 控制台左上角菜单进入 Identity & Security → Domains → Default（或你的身份域）→ Groups，" +
			"新建一个组，比如 " + group + "。",
		"进入该组 → Add user to group，把面板使用的那个用户加进去。" +
			"（用 API 密钥接入的用户就是密钥的属主，别加成别的用户。）",
		"进入 Identity → Policies，选中你的 tenancy（根 compartment），" +
			"点 Create Policy，名字随意，比如 oci-tools-minimal。",
		"把下面 8 条粘进策略编辑器（Policy Builder 切到 JSON 或直接粘贴文本均可），保存。" +
			"策略通常立即生效，偶尔需要等几十秒。",
	}
}

// validGroupName 粗校验组名。
//
// OCI 的组名允许字母、数字、空格、下划线、点、@、-、+。这里只放行这些，
// 并限制长度。挡住引号、分号、换行这类能改变策略语句结构的东西。
func validGroupName(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ' || r == '_' || r == '.' || r == '@' || r == '-' || r == '+':
		default:
			return false
		}
	}
	return true
}

// PolicyForCapability 返回覆盖某一族能力的策略行。
//
// 能力探测发现缺权限时会调它，把「该补哪条」直接说清楚——
// 让用户拿着一个族名去翻 IAM 文档，等于把问题推回去。
func PolicyForCapability(key, group string) string {
	if strings.TrimSpace(group) == "" {
		group = PolicyGroupPlaceholder
	} else if !validGroupName(group) {
		group = PolicyGroupPlaceholder
	}
	for _, pl := range policyLines {
		for _, c := range pl.caps {
			if c == key && pl.readOnly {
				// 优先给只读那条：探测只需要读权限，
				// 先补最小的一条就能让功能可用。
				return fmt.Sprintf("Allow group %s to %s %s %s", group, pl.verb, pl.family, PolicyScope)
			}
		}
	}
	return ""
}
