package opssvc

import (
	"context"
	"strings"
	"testing"

	"ocicore/internal/ociclient"
)

// fakeNet 记录 PUT 收到的完整规则集，用于断言「只增不减」。
type fakeNet struct {
	list       *ociclient.SecurityList
	putCalls   int
	putIngress []ociclient.IngressSecurityRule
	putEgress  []ociclient.EgressSecurityRule
}

func (f *fakeNet) GetSecurityList(_ context.Context, _, _ string) (*ociclient.SecurityList, error) {
	return f.list, nil
}

func (f *fakeNet) UpdateSecurityList(_ context.Context, _, _ string,
	ingress []ociclient.IngressSecurityRule, egress []ociclient.EgressSecurityRule,
) (*ociclient.SecurityList, error) {
	f.putCalls++
	f.putIngress = ingress
	f.putEgress = egress
	out := *f.list
	out.IngressSecurityRules = ingress
	out.EgressSecurityRules = egress
	return &out, nil
}

func tcpRule(port int) ociclient.IngressSecurityRule {
	return ociclient.IngressSecurityRule{
		Protocol:   "6",
		Source:     "0.0.0.0/0",
		SourceType: "CIDR_BLOCK",
		TCPOptions: &ociclient.TCPOptions{
			DestinationPortRange: &ociclient.PortRange{Min: port, Max: port},
		},
		Description: "原有规则",
	}
}

// 最要紧的一条：追加规则时原有规则必须一条不丢，PUT 的规则数只能增不能减。
func TestApplyOpenPortsOnlyAdds(t *testing.T) {
	original := tcpRule(22)
	egress := []ociclient.EgressSecurityRule{
		{Protocol: "all", Destination: "0.0.0.0/0", DestinationType: "CIDR_BLOCK"},
	}
	f := &fakeNet{list: &ociclient.SecurityList{
		ID:                   "sl-1",
		IngressSecurityRules: []ociclient.IngressSecurityRule{original},
		EgressSecurityRules:  egress,
	}}

	spec := OpenPortsSpec{
		Region:         "ap-tokyo-1",
		SecurityListID: "sl-1",
		Rules: []PortRule{
			{Protocol: "6", Port: 443}, // 新
			{Protocol: "6", Port: 22},  // 已存在
		},
	}
	msg, err := applyOpenPorts(context.Background(), f, spec)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if f.putCalls != 1 {
		t.Fatalf("PUT 次数 = %d，期望 1", f.putCalls)
	}
	if len(f.putIngress) != 2 {
		t.Fatalf("写回入站规则数 = %d，期望 2（原有 1 + 新增 1）", len(f.putIngress))
	}
	// 原有规则必须还在，且顺序保持在前。
	if ingressKey(f.putIngress[0]) != ingressKey(original) {
		t.Fatalf("原有规则丢失或被改动: %+v", f.putIngress[0])
	}
	// 出站规则必须原样带回，否则会掐断子网出网。
	if len(f.putEgress) != 1 || f.putEgress[0].Protocol != "all" {
		t.Fatalf("出站规则未原样带回: %+v", f.putEgress)
	}
	if !strings.Contains(msg, "新增 1 条") || !strings.Contains(msg, "跳过 1 条") {
		t.Fatalf("结果文案不符合预期: %s", msg)
	}
}

// 全部已存在时不应该发 PUT：一次多余的 PUT 就是一次整体替换的机会。
func TestApplyOpenPortsIdempotentSkipsPut(t *testing.T) {
	f := &fakeNet{list: &ociclient.SecurityList{
		ID:                   "sl-1",
		IngressSecurityRules: []ociclient.IngressSecurityRule{tcpRule(443)},
		EgressSecurityRules: []ociclient.EgressSecurityRule{
			{Protocol: "all", Destination: "0.0.0.0/0"},
		},
	}}
	msg, err := applyOpenPorts(context.Background(), f, OpenPortsSpec{
		Region: "r", SecurityListID: "sl-1",
		Rules: []PortRule{{Protocol: "6", Port: 443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.putCalls != 0 {
		t.Fatalf("无新增时不应 PUT，实际 %d 次", f.putCalls)
	}
	if !strings.Contains(msg, "未做改动") {
		t.Fatalf("结果文案不符合预期: %s", msg)
	}
}

// 没有出站规则时必须给出警告：这种状态下实例能开机但上不了网。
func TestApplyOpenPortsWarnsNoEgress(t *testing.T) {
	f := &fakeNet{list: &ociclient.SecurityList{ID: "sl-1"}}
	msg, err := applyOpenPorts(context.Background(), f, OpenPortsSpec{
		Region: "r", SecurityListID: "sl-1",
		Rules: []PortRule{{Protocol: "6", Port: 80}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "没有出站规则") {
		t.Fatalf("应提示缺少出站规则，实际: %s", msg)
	}
}

func TestApplyOpenPortsRejectsBadSpec(t *testing.T) {
	f := &fakeNet{list: &ociclient.SecurityList{ID: "sl-1"}}
	cases := []struct {
		name string
		spec OpenPortsSpec
	}{
		{"缺区域", OpenPortsSpec{SecurityListID: "sl-1", Rules: []PortRule{{Protocol: "6", Port: 80}}}},
		{"缺安全列表", OpenPortsSpec{Region: "r", Rules: []PortRule{{Protocol: "6", Port: 80}}}},
		{"缺规则", OpenPortsSpec{Region: "r", SecurityListID: "sl-1"}},
		{"坏协议", OpenPortsSpec{Region: "r", SecurityListID: "sl-1", Rules: []PortRule{{Protocol: "99", Port: 80}}}},
		{"端口越界", OpenPortsSpec{Region: "r", SecurityListID: "sl-1", Rules: []PortRule{{Protocol: "6", Port: 70000}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := applyOpenPorts(context.Background(), f, c.spec); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

// 防御性校验：如果服务端返回的规则数比原来少，必须报错而不是当作成功。
func TestApplyOpenPortsDetectsRuleLoss(t *testing.T) {
	lossy := &lossyNet{fakeNet: fakeNet{list: &ociclient.SecurityList{
		ID:                   "sl-1",
		IngressSecurityRules: []ociclient.IngressSecurityRule{tcpRule(22)},
		EgressSecurityRules:  []ociclient.EgressSecurityRule{{Protocol: "all", Destination: "0.0.0.0/0"}},
	}}}
	_, err := applyOpenPorts(context.Background(), lossy, OpenPortsSpec{
		Region: "r", SecurityListID: "sl-1",
		Rules: []PortRule{{Protocol: "6", Port: 443}},
	})
	if err == nil || !strings.Contains(err.Error(), "疑似规则丢失") {
		t.Fatalf("应检测到规则丢失，实际 err=%v", err)
	}
}

// lossyNet 模拟「PUT 之后服务端返回的规则反而变少」这种异常。
type lossyNet struct{ fakeNet }

func (l *lossyNet) UpdateSecurityList(_ context.Context, _, _ string,
	_ []ociclient.IngressSecurityRule, egress []ociclient.EgressSecurityRule,
) (*ociclient.SecurityList, error) {
	l.putCalls++
	// 故意只返回 0 条入站规则。
	return &ociclient.SecurityList{EgressSecurityRules: egress}, nil
}
