package accountsvc

import (
	"strings"
	"testing"
)

// 模板必须是 8 条，且每条都是完整的策略语句。
//
// 数量写死在这里是刻意的：方案 §4.4.1 定的就是 8 条。加一条要同时
// 改文档与这里，否则「模板里有 9 条但文档说 8 条」会一直没人发现。
func TestPolicyTemplateHasEightLines(t *testing.T) {
	tpl := BuildPolicyTemplate("")
	if len(tpl.Lines) != 8 {
		t.Fatalf("策略条数 = %d，期望 8", len(tpl.Lines))
	}
	texts := strings.Split(tpl.Text, "\n")
	if len(texts) != 8 {
		t.Fatalf("文本行数 = %d，期望 8", len(texts))
	}
	for i, l := range tpl.Lines {
		if !strings.HasPrefix(l.Text, "Allow group ") {
			t.Errorf("第 %d 条不是策略语句: %q", i+1, l.Text)
		}
		if !strings.HasSuffix(l.Text, " in tenancy") {
			t.Errorf("第 %d 条作用域不是 tenancy: %q", i+1, l.Text)
		}
		if l.Purpose == "" {
			t.Errorf("第 %d 条缺少用途说明", i+1)
		}
		if l.Text != texts[i] {
			t.Errorf("第 %d 条与全文不一致: %q vs %q", i+1, l.Text, texts[i])
		}
	}
}

// 这是这张卡最重要的一条：模板里绝不能出现 IAM 写权限。
//
// 少了这三条，凭据泄露后的爆炸半径是「动资源」而不是「改权限」——
// 后者可以把攻击者自己加进管理员组，前者至少还能靠删密钥止血。
func TestPolicyTemplateHasNoIAMWrite(t *testing.T) {
	tpl := BuildPolicyTemplate("oci-tools")
	for _, l := range tpl.Lines {
		for _, forbidden := range []string{
			"manage policies", "manage users", "manage groups",
			"manage domains", "manage identity-provider", "use policies",
		} {
			if strings.Contains(l.Text, forbidden) {
				t.Errorf("模板里出现了 IAM 写权限 %q: %s", forbidden, l.Text)
			}
		}
		// 也不该有 all-resources：那等于把上面省下来的全还回去。
		if strings.Contains(l.Text, "all-resources") {
			t.Errorf("模板里出现了 all-resources: %s", l.Text)
		}
	}
}

func TestPolicyTemplatePlaceholder(t *testing.T) {
	tpl := BuildPolicyTemplate("")
	if tpl.Group != PolicyGroupPlaceholder {
		t.Fatalf("Group = %q，期望占位符", tpl.Group)
	}
	if !strings.Contains(tpl.Text, PolicyGroupPlaceholder) {
		t.Fatal("未填组名时应保留占位符")
	}

	// 空白也算没填。
	if got := BuildPolicyTemplate("   "); got.Group != PolicyGroupPlaceholder {
		t.Errorf("空白组名 = %q", got.Group)
	}

	tpl = BuildPolicyTemplate("oci-tools-admins")
	if tpl.Group != "oci-tools-admins" {
		t.Fatalf("Group = %q", tpl.Group)
	}
	if strings.Contains(tpl.Text, PolicyGroupPlaceholder) {
		t.Fatal("填了组名后不该再出现占位符")
	}
	if !strings.Contains(tpl.Text, "Allow group oci-tools-admins to read instances in tenancy") {
		t.Errorf("第一条不对: %s", tpl.Text)
	}
}

// 组名会拼进策略语句，不能让它改变语句结构。
//
// 这不是安全边界（策略最终由 OCI 校验），而是别让用户复制出一份
// 语法错误的文本、然后以为是面板写错了。
func TestPolicyTemplateRejectsUnsafeGroupName(t *testing.T) {
	for _, bad := range []string{
		"x; Allow group evil to manage all-resources in tenancy",
		"group\nAllow group evil to manage all-resources in tenancy",
		`"quoted"`,
		"a'b",
		"tab\there",
		strings.Repeat("a", 200),
	} {
		got := BuildPolicyTemplate(bad)
		if got.Group != PolicyGroupPlaceholder {
			t.Errorf("组名 %q 应被拒，实际 %q", bad, got.Group)
		}
		if strings.Contains(got.Text, "all-resources") {
			t.Errorf("组名 %q 注入成功: %s", bad, got.Text)
		}
	}
}

// 每一族能力都必须至少被一条策略覆盖。
//
// 否则能力探测说「缺 storage 权限」，用户去模板里找不到对应的那条——
// 两处说的是同一件事，不能对不上。
func TestCapabilityPoliciesMatchTemplate(t *testing.T) {
	tpl := BuildPolicyTemplate("oci-tools")
	byCap := map[string][]string{}
	for _, l := range tpl.Lines {
		for _, c := range l.Capabilities {
			byCap[c] = append(byCap[c], l.Text)
		}
	}

	for _, spec := range CapabilitySpecs() {
		lines, ok := byCap[spec.Key]
		if !ok || len(lines) == 0 {
			t.Errorf("能力族 %q 在模板里没有对应策略", spec.Key)
			continue
		}
		// 探测里给的 policy 文案必须能在模板里找到同一条。
		// 两处都提策略却写法不同，用户不知道该信哪个。
		var found bool
		for _, line := range lines {
			if strings.Contains(line, spec.Policy) {
				found = true
			}
		}
		if !found {
			t.Errorf("能力族 %q 的策略 %q 在模板里找不到对应的行: %v",
				spec.Key, spec.Policy, lines)
		}
	}
}

// 模板里的族名必须都是真实存在的能力族，不能有拼错的孤儿。
func TestPolicyTemplateCapabilityKeysValid(t *testing.T) {
	valid := map[string]bool{}
	for _, spec := range CapabilitySpecs() {
		valid[spec.Key] = true
	}
	for _, l := range BuildPolicyTemplate("").Lines {
		if len(l.Capabilities) == 0 {
			t.Errorf("策略没有关联任何能力族: %s", l.Text)
		}
		for _, c := range l.Capabilities {
			if !valid[c] {
				t.Errorf("策略关联了不存在的能力族 %q: %s", c, l.Text)
			}
		}
	}
}

// 缺权限时给的那一条应当是只读的：探测只需要读权限，
// 先补最小的一条就能让功能可用。
func TestPolicyForCapabilityPrefersReadOnly(t *testing.T) {
	got := PolicyForCapability(CapStorage, "oci-tools")
	if got != "Allow group oci-tools to read volume-family in tenancy" {
		t.Fatalf("= %q", got)
	}
	// 未填组名时保留占位符。
	if got := PolicyForCapability(CapCompute, ""); !strings.Contains(got, PolicyGroupPlaceholder) {
		t.Errorf("= %q", got)
	}
	// 未知族返回空串，不要编一条出来。
	if got := PolicyForCapability("nope", "g"); got != "" {
		t.Errorf("未知族应返回空串，实际 %q", got)
	}
}

// 四步说明要写具体菜单路径。
//
// 用户走到这一步时已经因为 403 而烦躁了，让他去翻文档找「Identity 在哪」
// 等于把问题推回去。
func TestPolicyTemplateStepsAreConcrete(t *testing.T) {
	tpl := BuildPolicyTemplate("oci-tools-admins")
	if len(tpl.Steps) != 4 {
		t.Fatalf("步骤数 = %d，期望 4", len(tpl.Steps))
	}
	joined := strings.Join(tpl.Steps, "\n")
	for _, want := range []string{"Groups", "Add user to group", "Policies", "Create Policy"} {
		if !strings.Contains(joined, want) {
			t.Errorf("步骤里缺少 %q:\n%s", want, joined)
		}
	}
	// 组名要出现在步骤里，用户不用自己替换两处。
	if !strings.Contains(joined, "oci-tools-admins") {
		t.Error("步骤里应带上组名")
	}
}

func TestPolicyTemplateReadOnlyCount(t *testing.T) {
	tpl := BuildPolicyTemplate("")
	// 5 条读 + 3 条写。
	if tpl.ReadOnly != 5 {
		t.Errorf("只读条数 = %d，期望 5", tpl.ReadOnly)
	}
}

// 说明文案要写清「没有 IAM 写权限」这件事——这是这套方案的核心卖点，
// 用户需要知道自己在换什么。
func TestPolicyTemplateNotice(t *testing.T) {
	tpl := BuildPolicyTemplate("")
	for _, want := range []string{"manage policies", "权限自检"} {
		if !strings.Contains(tpl.Notice, want) {
			t.Errorf("说明里应提到 %q", want)
		}
	}
	if tpl.Version == "" {
		t.Error("应带版本号")
	}
}
