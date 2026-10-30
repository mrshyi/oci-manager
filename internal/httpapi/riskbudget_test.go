package httpapi

import (
	"context"
	"net/http"
	"testing"

	"ocicore/internal/riskguard"
)

// budgetResponse 是 /api/risk-budget 的响应形状。
type budgetResponse struct {
	Enabled     bool         `json:"enabled"`
	Window      string       `json:"window"`
	WindowStart int64        `json:"windowStart"`
	Limit       int          `json:"limit"`
	Accounts    []budgetItem `json:"accounts"`
}

// TestRiskBudgetReportsUsage 验证预算视图把「已用 / 上限 / 倍数」都摆出来。
//
// 预算生效之后，用户会看到抢机任务从 60 秒一次变成 240 秒一次。如果界面上
// 没有任何地方说明原因，第一反应是「这工具卡了」——然后去重启它，于是预算
// 重新计数、间隔恢复，问题看起来"自己好了"，直到下次又变慢。
func TestRiskBudgetReportsUsage(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")

	// 未产生任何调用：一条账号记录，用量 0，不降频。
	resp := h.get("/api/risk-budget")
	expectStatus(t, resp, http.StatusOK)
	body := decode[budgetResponse](t, resp)
	if !body.Enabled {
		t.Fatal("接了预算时 enabled 应为 true")
	}
	if body.Limit != riskguard.DefaultHourlyLimit {
		t.Errorf("上限 = %d，期望 %d", body.Limit, riskguard.DefaultHourlyLimit)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].AccountID != acc.ID {
		t.Fatalf("账号列表 = %+v", body.Accounts)
	}
	if body.Accounts[0].Used != 0 || body.Accounts[0].Multiplier != 1 {
		t.Errorf("未调用时 used=%d multiplier=%d，期望 0/1",
			body.Accounts[0].Used, body.Accounts[0].Multiplier)
	}

	// 灌满预算：用量与倍数都要跟着动。
	for i := 0; i < riskguard.DefaultHourlyLimit; i++ {
		h.budget.Record(context.Background(), acc.ID)
	}
	resp = h.get("/api/risk-budget")
	expectStatus(t, resp, http.StatusOK)
	body = decode[budgetResponse](t, resp)
	if body.Accounts[0].Used != riskguard.DefaultHourlyLimit {
		t.Errorf("已用 = %d，期望 %d", body.Accounts[0].Used, riskguard.DefaultHourlyLimit)
	}
	if body.Accounts[0].Multiplier != 2 {
		t.Errorf("用满预算后倍数 = %d，期望 2", body.Accounts[0].Multiplier)
	}
}

// TestRiskBudgetReportsDisabled 验证未接预算时明确返回 enabled=false。
//
// 返回空列表会被读成「所有账号都是 0 次」，让人误以为统计在工作——
// 而实际上降频永远不会触发。
func TestRiskBudgetReportsDisabled(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	// 直接把预算摘掉，模拟未接预算的部署。
	h.api.budget = nil

	resp := h.get("/api/risk-budget")
	expectStatus(t, resp, http.StatusOK)
	body := decode[budgetResponse](t, resp)
	if body.Enabled {
		t.Fatal("未接预算时 enabled 应为 false")
	}
	if len(body.Accounts) != 0 {
		t.Errorf("未接预算时不应返回账号用量，实际 %+v", body.Accounts)
	}
}
