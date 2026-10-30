package httpapi

import (
	"net/http"
	"testing"

	"ocicore/internal/store"
)

// accountsEnvelope 是 GET /api/accounts 的响应。
type accountsEnvelope struct {
	Accounts []struct {
		ID         string   `json:"id"`
		Alias      string   `json:"alias"`
		Tags       []string `json:"tags"`
		ExpiryDays *int     `json:"expiryDays"`
		Meta       struct {
			GroupName  string `json:"groupName"`
			Note       string `json:"note"`
			SortWeight int    `json:"sortWeight"`
		} `json:"meta"`
	} `json:"accounts"`
	Groups []string `json:"groups"`
	Tags   []struct {
		Tag   string `json:"tag"`
		Count int    `json:"count"`
	} `json:"tags"`
}

type tagsEnvelope struct {
	Tags []string `json:"tags"`
}

func TestAccountTagsRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TYO", "tags")

	// 没打过标签时是空数组而不是 null。
	resp := h.get("/api/accounts/" + acc.ID + "/tags")
	expectStatus(t, resp, http.StatusOK)
	empty := decode[tagsEnvelope](t, resp)
	if empty.Tags == nil || len(empty.Tags) != 0 {
		t.Fatalf("未打标签时应当是空数组，实际 %v", empty.Tags)
	}

	resp = h.do("PUT", "/api/accounts/"+acc.ID+"/tags",
		map[string]any{"tags": []string{" 东京 ", "主力", "东京", ""}}, true)
	expectStatus(t, resp, http.StatusOK)
	out := decode[tagsEnvelope](t, resp)
	// 去空白、去重、丢空值之后剩两个。
	if len(out.Tags) != 2 || out.Tags[0] != "东京" || out.Tags[1] != "主力" {
		t.Fatalf("清洗后的标签不对：%v", out.Tags)
	}

	// 列表接口要把标签带出来，前端才不用为每个账号再发一次请求。
	resp = h.get("/api/accounts")
	expectStatus(t, resp, http.StatusOK)
	list := decode[accountsEnvelope](t, resp)
	if len(list.Accounts) != 1 {
		t.Fatalf("账号数量 = %d", len(list.Accounts))
	}
	if len(list.Accounts[0].Tags) != 2 {
		t.Fatalf("列表里的标签不对：%v", list.Accounts[0].Tags)
	}
	if len(list.Tags) != 2 {
		t.Fatalf("标签统计不对：%v", list.Tags)
	}
	// 筛选器要用的清单与账号上的标签必须一致，否则会出现
	// 「筛选器里有 A，但点下去一个账号都不剩」。
	for _, u := range list.Tags {
		if u.Count != 1 {
			t.Fatalf("标签 %s 的计数应当是 1，实际 %d", u.Tag, u.Count)
		}
	}
}

func TestAccountTagsValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TYO", "tagsv")

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"标签过长", map[string]any{"tags": []string{
			"一二三四五六七八九十一二三四五六七八九十一二三四五",
		}}, http.StatusBadRequest},
		{"控制字符", map[string]any{"tags": []string{"东京\u0000"}}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.do("PUT", "/api/accounts/"+acc.ID+"/tags", c.body, true)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d，期望 %d", resp.StatusCode, c.want)
			}
			resp.Body.Close()
		})
	}

	// 账号不存在是 404，不是 500——外键失败如果直接冒出去会被翻译成服务器错误。
	resp := h.do("PUT", "/api/accounts/nope/tags", map[string]any{"tags": []string{"x"}}, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("给不存在的账号打标签应当是 404，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAccountMetaRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TYO", "meta")

	resp := h.get("/api/accounts/" + acc.ID + "/meta")
	expectStatus(t, resp, http.StatusOK)
	empty := decode[struct {
		Meta struct {
			GroupName string `json:"groupName"`
			Note      string `json:"note"`
		} `json:"meta"`
	}](t, resp)
	if empty.Meta.GroupName != "" {
		t.Fatalf("未设置时分组名应当为空，实际 %q", empty.Meta.GroupName)
	}

	// 只给日期（<input type="date"> 的输出格式）也必须能收。
	resp = h.do("PUT", "/api/accounts/"+acc.ID+"/meta", map[string]any{
		"groupName":  " 日本 ",
		"note":       "主力号",
		"expiresAt":  "2026-12-31",
		"sortWeight": 10,
	}, true)
	expectStatus(t, resp, http.StatusOK)
	saved := decode[struct {
		Meta struct {
			GroupName  string  `json:"groupName"`
			Note       string  `json:"note"`
			SortWeight int     `json:"sortWeight"`
			ExpiresAt  *string `json:"expiresAt"`
		} `json:"meta"`
	}](t, resp)
	if saved.Meta.GroupName != "日本" {
		t.Errorf("分组名应当被 trim，实际 %q", saved.Meta.GroupName)
	}
	if saved.Meta.Note != "主力号" || saved.Meta.SortWeight != 10 {
		t.Errorf("保存的值不对：%+v", saved.Meta)
	}
	if saved.Meta.ExpiresAt == nil {
		t.Error("到期时间应当被保存")
	}

	// 列表接口带出 meta 与派生出来的「还剩几天」。
	resp = h.get("/api/accounts")
	expectStatus(t, resp, http.StatusOK)
	list := decode[accountsEnvelope](t, resp)
	if len(list.Accounts) != 1 {
		t.Fatalf("账号数量 = %d", len(list.Accounts))
	}
	if list.Accounts[0].Meta.GroupName != "日本" {
		t.Errorf("列表里的分组名不对：%+v", list.Accounts[0].Meta)
	}
	if list.Accounts[0].ExpiryDays == nil {
		t.Error("列表应当带上「还剩几天到期」")
	}
	if len(list.Groups) != 1 || list.Groups[0] != "日本" {
		t.Errorf("分组清单不对：%v", list.Groups)
	}

	// 整体替换：这次不传 expiresAt，到期时间应当被清掉。
	resp = h.do("PUT", "/api/accounts/"+acc.ID+"/meta", map[string]any{
		"groupName": "日本",
	}, true)
	expectStatus(t, resp, http.StatusOK)
	cleared := decode[struct {
		Meta struct {
			ExpiresAt *string `json:"expiresAt"`
		} `json:"meta"`
	}](t, resp)
	if cleared.Meta.ExpiresAt != nil {
		t.Errorf("没传的字段应当被清空，实际 %v", *cleared.Meta.ExpiresAt)
	}
}

func TestAccountMetaValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TYO", "metav")

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"时间格式错", map[string]any{"expiresAt": "明年"}, http.StatusBadRequest},
		{"权重越界", map[string]any{"sortWeight": 5000}, http.StatusBadRequest},
		{"分组名过长", map[string]any{
			"groupName": "一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十一二三",
		}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.do("PUT", "/api/accounts/"+acc.ID+"/meta", c.body, true)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d，期望 %d", resp.StatusCode, c.want)
			}
			resp.Body.Close()
		})
	}

	resp := h.do("PUT", "/api/accounts/nope/meta", map[string]any{"groupName": "x"}, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的账号应当是 404，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// 写操作要记审计：标签和备注本身不危险，但"谁把某个账号归到了哪一组"
// 是事后追查时需要的信息。
func TestAccountOrganizeIsAudited(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京号", "TYO", "audit")

	resp := h.do("PUT", "/api/accounts/"+acc.ID+"/tags",
		map[string]any{"tags": []string{"东京"}}, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	logs, _, err := h.store.ListAudit(t.Context(), store.AuditFilter{AccountID: acc.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range logs {
		if l.Action == "account_tags" && l.AccountID == acc.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("改标签没有留下审计记录")
	}
}
