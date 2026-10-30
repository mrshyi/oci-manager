package config

import "testing"

func TestParseAllowedNets(t *testing.T) {
	nets, err := parseAllowedNets(" 10.0.0.0/8, 192.168.1.5 ,fd00::/8,")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.5/32", "fd00::/8"}
	if len(nets) != len(want) {
		t.Fatalf("解析结果 %v，期望 %v", nets, want)
	}
	for i, w := range want {
		if nets[i].String() != w {
			t.Errorf("第 %d 项 = %s，期望 %s", i, nets[i], w)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip"} {
		if _, err := parseAllowedNets(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

// TestRejectUnboundedPrefix 锁住「不许写 0.0.0.0/0」这条启动校验。
//
// 写这两个网段的人几乎一定是想表达「我信任我的代理」，但实际含义是
// 「我信任全世界」——配上 X-Forwarded-For 就等于把登录限流的桶交出去。
// 这种配置错误不会报错、不会有症状，只会在被爆破之后才从审计日志里
// 看到一串编造的 IP，所以必须在启动时就拦住。
func TestRejectUnboundedPrefix(t *testing.T) {
	for _, bad := range []string{"0.0.0.0/0", "::/0"} {
		nets, err := parseAllowedNets(bad)
		if err != nil {
			t.Fatalf("%q 应当能解析: %v", bad, err)
		}
		if err := rejectUnboundedPrefix(nets); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}

	for _, ok := range []string{"10.0.0.0/8", "127.0.0.1", "::1/128", "172.16.0.0/12"} {
		nets, err := parseAllowedNets(ok)
		if err != nil {
			t.Fatalf("%q 应当能解析: %v", ok, err)
		}
		if err := rejectUnboundedPrefix(nets); err != nil {
			t.Errorf("%q 不应被拒绝: %v", ok, err)
		}
	}
}

// TestTrustedProxiesFromEnv 验证新变量优先、旧布尔开关仍有合理回落。
func TestTrustedProxiesFromEnv(t *testing.T) {
	t.Setenv("OCICORE_TRUSTED_PROXIES", "127.0.0.0/8,10.0.0.0/8")
	t.Setenv("OCICORE_TRUST_PROXY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("可信代理数量 = %d，期望 2", len(cfg.TrustedProxies))
	}

	// 显式网段优先于旧的布尔开关，两者同时存在时以网段为准。
	t.Setenv("OCICORE_TRUST_PROXY", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("显式网段应优先，实际 %d 项", len(cfg.TrustedProxies))
	}

	// 什么都不配：不信任任何代理，XFF 一律不看。
	t.Setenv("OCICORE_TRUSTED_PROXIES", "")
	t.Setenv("OCICORE_TRUST_PROXY", "")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("默认不应信任任何代理，实际 %v", cfg.TrustedProxies)
	}

	// 旧开关打开：回落到「回环 + 私有网段」，行为基本不变但不能是 0.0.0.0/0。
	t.Setenv("OCICORE_TRUST_PROXY", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) == 0 {
		t.Fatal("旧开关打开时应回落到一组具体网段")
	}
	for _, p := range cfg.TrustedProxies {
		if p.Bits() == 0 {
			t.Fatalf("回落网段里出现了 %s，等于信任所有来源", p)
		}
	}
}

// TestTrustedProxiesRejectsUnbounded 验证 0.0.0.0/0 会让 Load 直接失败。
func TestTrustedProxiesRejectsUnbounded(t *testing.T) {
	t.Setenv("OCICORE_TRUSTED_PROXIES", "0.0.0.0/0")
	if _, err := Load(); err == nil {
		t.Fatal("配置 0.0.0.0/0 时 Load 应当失败")
	}
}

// TestRiskBudgetEnabledDefaultsOn 锁住「风控开关默认开」这条。
//
// 默认值的取向必须偏保守：漏配的代价是「没降频」，而风控类保护恰恰是
// 最不该依赖用户主动打开的那一类——会去打开它的人，本来就不太需要它。
func TestRiskBudgetEnabledDefaultsOn(t *testing.T) {
	t.Setenv("OCICORE_RISK_BUDGET_ENABLED", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RiskBudgetEnabled {
		t.Fatal("未配置时应默认开启调用预算")
	}

	for _, off := range []string{"false", "0", "no"} {
		t.Setenv("OCICORE_RISK_BUDGET_ENABLED", off)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RiskBudgetEnabled {
			t.Errorf("%q 应当关闭调用预算", off)
		}
	}

	// 写成无法解析的值时保持开启：配置写错不该顺手把保护关掉。
	t.Setenv("OCICORE_RISK_BUDGET_ENABLED", "maybe")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RiskBudgetEnabled {
		t.Error("无法解析的值应保持开启")
	}
}
