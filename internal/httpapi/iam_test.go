package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"ocicore/internal/accountsvc"
)

func TestPolicyTemplateEndpoint(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/iam/policy-template")
	expectStatus(t, resp, http.StatusOK)
	got := decode[accountsvc.PolicyTemplate](t, resp)

	if len(got.Lines) != 8 {
		t.Fatalf("策略条数 = %d，期望 8", len(got.Lines))
	}
	if got.Group != accountsvc.PolicyGroupPlaceholder {
		t.Errorf("未传组名时应保留占位符，实际 %q", got.Group)
	}
	if len(got.Steps) != 4 {
		t.Errorf("步骤数 = %d", len(got.Steps))
	}
	if !strings.Contains(got.Text, "Allow group") {
		t.Errorf("全文不对: %s", got.Text)
	}
}

// 组名从查询参数来，替换后全文与逐条都要跟着变。
func TestPolicyTemplateEndpointWithGroup(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/iam/policy-template?group=oci-tools-admins")
	expectStatus(t, resp, http.StatusOK)
	got := decode[accountsvc.PolicyTemplate](t, resp)

	if got.Group != "oci-tools-admins" {
		t.Fatalf("Group = %q", got.Group)
	}
	if strings.Contains(got.Text, accountsvc.PolicyGroupPlaceholder) {
		t.Error("填了组名后不该再出现占位符")
	}
	for _, l := range got.Lines {
		if !strings.Contains(l.Text, "oci-tools-admins") {
			t.Errorf("这一条没带上组名: %s", l.Text)
		}
	}
}

// 注入尝试要被挡在占位符上，不能让查询参数改变策略语句结构。
func TestPolicyTemplateEndpointRejectsInjection(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/iam/policy-template?group=x;%20Allow%20group%20evil%20to%20manage%20all-resources%20in%20tenancy")
	expectStatus(t, resp, http.StatusOK)
	got := decode[accountsvc.PolicyTemplate](t, resp)

	if strings.Contains(got.Text, "all-resources") {
		t.Fatalf("组名注入成功: %s", got.Text)
	}
	if got.Group != accountsvc.PolicyGroupPlaceholder {
		t.Errorf("非法组名应退回占位符，实际 %q", got.Group)
	}
}

func TestPolicyTemplateRequiresAuth(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/api/iam/policy-template")
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}
