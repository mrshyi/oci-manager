package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"ocicore/internal/store"
)

// taskEnvelope 只解出测试关心的字段。
//
// 不直接解成 store.OpsTask：DTO 把 progress 从 JSON 字符串换成了对象，
// 直接按 OpsTask 解会在 progress 上类型不匹配。
type taskEnvelope struct {
	Task struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		KindLabel string `json:"kindLabel"`
	} `json:"task"`
	Notice string `json:"notice"`
}

func TestCreateOpenPortsTask(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "openports")

	resp := h.post("/api/ops/open-ports", map[string]any{
		"accountId":      acc.ID,
		"region":         "ap-tokyo-1",
		"securityListId": "ocid1.securitylist.oc1..sl",
		"source":         "0.0.0.0/0",
		"rules": []map[string]any{
			{"protocol": "6", "port": 80},
			{"protocol": "6", "port": 443, "description": "https"},
		},
	})
	expectStatus(t, resp, http.StatusCreated)
	out := decode[taskEnvelope](t, resp)

	if out.Task.Kind != store.OpsKindOpenPorts {
		t.Errorf("kind = %q，期望 %q", out.Task.Kind, store.OpsKindOpenPorts)
	}
	if out.Task.State != store.OpsPending {
		t.Errorf("新任务应为 pending，实际 %q", out.Task.State)
	}
	if out.Task.AccountID != acc.ID {
		t.Errorf("accountId = %q，期望 %q", out.Task.AccountID, acc.ID)
	}
	if out.Task.Name == "" {
		t.Error("任务名不应为空")
	}
	if out.Task.KindLabel != "放行端口" {
		t.Errorf("kindLabel = %q，期望「放行端口」", out.Task.KindLabel)
	}
	if out.Notice == "" {
		t.Error("应返回一句操作说明")
	}

	// 列表里应能按 kind 过滤到它。
	resp = h.get("/api/tasks?kind=open_ports")
	expectStatus(t, resp, http.StatusOK)
	list := decode[struct {
		Tasks []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"tasks"`
	}](t, resp)
	if len(list.Tasks) != 1 || list.Tasks[0].ID != out.Task.ID {
		t.Fatalf("列表未包含新建任务: %+v", list.Tasks)
	}
}

func TestCreateOpenPortsTaskValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "opv")

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{
			"缺规则",
			map[string]any{"accountId": acc.ID, "region": "r", "securityListId": "sl"},
			http.StatusBadRequest,
		},
		{
			"缺区域",
			map[string]any{"accountId": acc.ID, "securityListId": "sl",
				"rules": []map[string]any{{"protocol": "6", "port": 80}}},
			http.StatusBadRequest,
		},
		{
			"坏协议",
			map[string]any{"accountId": acc.ID, "region": "r", "securityListId": "sl",
				"rules": []map[string]any{{"protocol": "99", "port": 80}}},
			http.StatusBadRequest,
		},
		{
			"账号不存在",
			map[string]any{"accountId": "nope", "region": "r", "securityListId": "sl",
				"rules": []map[string]any{{"protocol": "6", "port": 80}}},
			http.StatusNotFound,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.post("/api/ops/open-ports", c.body)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d，期望 %d", resp.StatusCode, c.want)
			}
			resp.Body.Close()
		})
	}
}

func TestCreateChangeIPTask(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "changeip")

	resp := h.post("/api/ops/change-ip", map[string]any{
		"accountId":          acc.ID,
		"region":             "ap-tokyo-1",
		"vnicId":             "ocid1.vnic.oc1..v",
		"compartmentId":      "ocid1.compartment.oc1..c",
		"targetCidrs":        []string{"203.0.113.0/24"},
		"maxAttempts":        5,
		"maxDurationMinutes": 10,
	})
	expectStatus(t, resp, http.StatusCreated)
	out := decode[taskEnvelope](t, resp)

	if out.Task.Kind != store.OpsKindChangeIP {
		t.Errorf("kind = %q，期望 %q", out.Task.Kind, store.OpsKindChangeIP)
	}
	if out.Task.State != store.OpsPending {
		t.Errorf("新任务应为 pending，实际 %q", out.Task.State)
	}
	if out.Task.AccountID != acc.ID {
		t.Errorf("accountId = %q，期望 %q", out.Task.AccountID, acc.ID)
	}
	if !strings.Contains(out.Notice, "5 次") || !strings.Contains(out.Notice, "10 分钟") {
		t.Errorf("说明应包含次数与时长上限: %s", out.Notice)
	}
}

// rescueEnvelope 在 taskEnvelope 之外还要拿阶段清单。
type rescueEnvelope struct {
	taskEnvelope
	Stages []string `json:"stages"`
}

func TestCreateRescueTask(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "rescue")

	const (
		target = "ocid1.instance.oc1.ap-tokyo-1.antarget001"
		rescue = "ocid1.instance.oc1.ap-tokyo-1.anrescue01"
	)
	resp := h.post("/api/ops/rescue", map[string]any{
		"accountId":        acc.ID,
		"region":           "ap-tokyo-1",
		"targetInstanceId": target,
		"rescueInstanceId": rescue,
		"compartmentId":    "ocid1.compartment.oc1..c",
	})
	expectStatus(t, resp, http.StatusCreated)
	out := decode[rescueEnvelope](t, resp)

	if out.Task.Kind != store.OpsKindRescue {
		t.Errorf("kind = %q，期望 %q", out.Task.Kind, store.OpsKindRescue)
	}
	if out.Task.State != store.OpsPending {
		t.Errorf("新任务应为 pending，实际 %q", out.Task.State)
	}
	if out.Task.KindLabel != "救援编排" {
		t.Errorf("kindLabel = %q，期望「救援编排」", out.Task.KindLabel)
	}
	// 任务名要能认出是哪台机器，同时不能把整条 OCID 塞进去。
	if !strings.Contains(out.Task.Name, "antarget001") {
		t.Errorf("任务名应包含目标实例 ID 末段，实际 %q", out.Task.Name)
	}
	if strings.Contains(out.Task.Name, "ocid1.instance") {
		t.Errorf("任务名不该包含完整 OCID，实际 %q", out.Task.Name)
	}
	// 阶段清单随创建结果一起给，前端不必自己维护一份。
	if len(out.Stages) != 9 || out.Stages[0] != "PRECHECK" || out.Stages[8] != "DONE" {
		t.Errorf("阶段清单不对: %v", out.Stages)
	}
	if !strings.Contains(out.Notice, "继续") {
		t.Errorf("说明应提示需要在等待点手动继续: %s", out.Notice)
	}
}

func TestCreateRescueTaskValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "rescuev")

	base := func() map[string]any {
		return map[string]any{
			"accountId":        acc.ID,
			"region":           "ap-tokyo-1",
			"targetInstanceId": "ocid1.instance.oc1..t",
			"rescueInstanceId": "ocid1.instance.oc1..r",
			"compartmentId":    "ocid1.compartment.oc1..c",
		}
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{"缺 accountId", func(b map[string]any) { delete(b, "accountId") }, http.StatusBadRequest},
		{"缺目标实例", func(b map[string]any) { delete(b, "targetInstanceId") }, http.StatusBadRequest},
		{"缺救援机", func(b map[string]any) { delete(b, "rescueInstanceId") }, http.StatusBadRequest},
		{"缺 compartment", func(b map[string]any) { delete(b, "compartmentId") }, http.StatusBadRequest},
		// 自己救自己是没有意义且危险的组合：流程会把这台机器关机再去挂它的卷。
		{"目标与救援机相同", func(b map[string]any) {
			b["rescueInstanceId"] = b["targetInstanceId"]
		}, http.StatusBadRequest},
		{"账号不存在", func(b map[string]any) { b["accountId"] = "nope" }, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := base()
			c.mutate(body)
			resp := h.post("/api/ops/rescue", body)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d，期望 %d", resp.StatusCode, c.want)
			}
			resp.Body.Close()
		})
	}
}

func TestCreateBandwidthTask(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "bw")

	const inst = "ocid1.instance.oc1.ap-tokyo-1.anbwinst01"
	resp := h.post("/api/ops/bandwidth", map[string]any{
		"accountId":     acc.ID,
		"region":        "ap-tokyo-1",
		"compartmentId": "ocid1.compartment.oc1..c",
		"instanceId":    inst,
		"action":        "enable",
	})
	expectStatus(t, resp, http.StatusCreated)
	out := decode[rescueEnvelope](t, resp)

	if out.Task.Kind != store.OpsKindEnable500 {
		t.Errorf("kind = %q，期望 %q", out.Task.Kind, store.OpsKindEnable500)
	}
	if out.Task.State != store.OpsPending {
		t.Errorf("新任务应为 pending，实际 %q", out.Task.State)
	}
	if !strings.Contains(out.Task.Name, "开启") || !strings.Contains(out.Task.Name, "anbwinst01") {
		t.Errorf("任务名应同时含动作与实例末段，实际 %q", out.Task.Name)
	}
	if len(out.Stages) == 0 || out.Stages[0] != "PRECHECK" || out.Stages[len(out.Stages)-1] != "DONE" {
		t.Errorf("开启阶段清单不对: %v", out.Stages)
	}
	// NLB 会产生费用，说明里必须写明。
	if !strings.Contains(out.Notice, "NAT") {
		t.Errorf("说明应交代会创建哪些资源: %s", out.Notice)
	}

	// 关闭：阶段清单换成逆序，任务名带「关闭」。
	resp = h.post("/api/ops/bandwidth", map[string]any{
		"accountId":     acc.ID,
		"region":        "ap-tokyo-1",
		"compartmentId": "ocid1.compartment.oc1..c",
		"instanceId":    inst,
		"action":        "disable",
	})
	expectStatus(t, resp, http.StatusCreated)
	off := decode[rescueEnvelope](t, resp)
	if !strings.Contains(off.Task.Name, "关闭") {
		t.Errorf("关闭任务名应含「关闭」，实际 %q", off.Task.Name)
	}
	if off.Stages[0] != "DISCOVER" || off.Stages[len(off.Stages)-1] != "CLEANED" {
		t.Errorf("关闭阶段清单不对: %v", off.Stages)
	}
	if !strings.Contains(off.Notice, "逆序") {
		t.Errorf("关闭说明应交代逆序清理: %s", off.Notice)
	}
}

func TestCreateBandwidthTaskValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "bwv")

	base := func() map[string]any {
		return map[string]any{
			"accountId":     acc.ID,
			"region":        "ap-tokyo-1",
			"compartmentId": "ocid1.compartment.oc1..c",
			"instanceId":    "ocid1.instance.oc1..i",
			"action":        "enable",
		}
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{"缺 accountId", func(b map[string]any) { delete(b, "accountId") }, http.StatusBadRequest},
		{"缺区域", func(b map[string]any) { delete(b, "region") }, http.StatusBadRequest},
		{"缺 compartment", func(b map[string]any) { delete(b, "compartmentId") }, http.StatusBadRequest},
		{"缺实例", func(b map[string]any) { delete(b, "instanceId") }, http.StatusBadRequest},
		{"未知动作", func(b map[string]any) { b["action"] = "reboot" }, http.StatusBadRequest},
		{"端口越界", func(b map[string]any) { b["listenerPort"] = 70000 }, http.StatusBadRequest},
		{"账号不存在", func(b map[string]any) { b["accountId"] = "nope" }, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := base()
			c.mutate(body)
			resp := h.post("/api/ops/bandwidth", body)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d，期望 %d", resp.StatusCode, c.want)
			}
			resp.Body.Close()
		})
	}
}

func TestCreateChangeIPTaskValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TK", "cipv")

	base := func() map[string]any {
		return map[string]any{
			"accountId":     acc.ID,
			"region":        "ap-tokyo-1",
			"vnicId":        "ocid1.vnic.oc1..v",
			"compartmentId": "ocid1.compartment.oc1..c",
			"targetCidrs":   []string{"203.0.113.0/24"},
		}
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{"缺 accountId", func(b map[string]any) { delete(b, "accountId") }, http.StatusBadRequest},
		{"缺网卡", func(b map[string]any) { delete(b, "vnicId") }, http.StatusBadRequest},
		{"缺目标网段", func(b map[string]any) { delete(b, "targetCidrs") }, http.StatusBadRequest},
		{"坏网段", func(b map[string]any) { b["targetCidrs"] = []string{"not-cidr"} }, http.StatusBadRequest},
		{"账号不存在", func(b map[string]any) { b["accountId"] = "nope" }, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := base()
			c.mutate(body)
			resp := h.post("/api/ops/change-ip", body)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d，期望 %d", resp.StatusCode, c.want)
			}
			resp.Body.Close()
		})
	}
}
