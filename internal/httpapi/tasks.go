package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"ocicore/internal/store"
)

// 运维任务的对外接口。
//
// 这一层目前是"只有读和状态切换、还没有生产者"：P1-2 起各类运维作业
// （放行端口、换 IP、救援）才会往 ops_tasks 里写。先把表、状态机与查询
// 接口做出来，是为了让后续每个作业类型只关心自己的业务，不必各自再想
// 一遍"状态怎么变、进度怎么写、暂停了怎么办"。

// opsKindLabels 是任务类型的中文名。空类型返回 kind 原文——
// 显示一个陌生的英文串，比显示空白更容易让人去查日志。
var opsKindLabels = map[string]string{
	store.OpsKindChangeIP:  "更换公网 IP",
	store.OpsKindOpenPorts: "放行端口",
	store.OpsKindEnable500: "提升带宽至 500Mbps",
	store.OpsKindRescue:    "救援编排",
	store.OpsKindBulk:      "批量操作",
}

// opsTaskDTO 是任务的对外表示。
//
// 覆盖 Progress 是因为库里存的是 JSON 字符串，直接透出去前端还要再解一层；
// 解析失败时给空对象而不是报错——进度条画不出来不该让整个列表 500。
type opsTaskDTO struct {
	store.OpsTask
	Progress     map[string]any `json:"progress"`
	AccountAlias string         `json:"accountAlias"`
	KindLabel    string         `json:"kindLabel"`
}

func toOpsDTO(t store.OpsTask, aliases map[string]string) opsTaskDTO {
	dto := opsTaskDTO{
		OpsTask:      t,
		Progress:     map[string]any{},
		AccountAlias: aliases[t.AccountID],
		KindLabel:    opsKindLabels[t.Kind],
	}
	if dto.KindLabel == "" {
		dto.KindLabel = t.Kind
	}
	if raw := strings.TrimSpace(t.Progress); raw != "" {
		if err := json.Unmarshal([]byte(raw), &dto.Progress); err != nil {
			slog.Debug("运维任务进度不是合法 JSON", "task", t.ID, "err", err)
			dto.Progress = map[string]any{}
		}
	}
	return dto
}

// accountAliases 返回账号 ID → 别名。查不到就返回空表：列表的主体是任务，
// 别名只是辅助显示，为它失败整页没有道理。
func (s *Server) accountAliases(ctx context.Context) map[string]string {
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		slog.Warn("运维任务列表：读取账号别名失败", "err", err)
		return nil
	}
	out := make(map[string]string, len(accounts))
	for _, acc := range accounts {
		out[acc.ID] = acc.Alias
	}
	return out
}

func (s *Server) handleListOpsTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tasks, err := s.st.ListOpsTasks(r.Context(), store.OpsTaskFilter{
		AccountID: q.Get("accountId"),
		Kind:      q.Get("kind"),
		State:     q.Get("state"),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	aliases := s.accountAliases(r.Context())
	out := make([]opsTaskDTO, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toOpsDTO(t, aliases))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": out,
		// 状态清单随列表一起给：前端不必自己维护一份，将来加状态也不会漏改。
		"states": []string{
			store.OpsPending, store.OpsRunning, store.OpsSucceeded,
			store.OpsFailed, store.OpsPaused, store.OpsCanceled,
		},
	})
}

func (s *Server) handleGetOpsTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.st.GetOpsTask(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrOpsTaskNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "任务不存在")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task": toOpsDTO(*task, s.accountAliases(r.Context())),
	})
}

// opsActions 把动作名映射到目标状态。
//
// 这里只描述"想变成什么"，合法与否由 store 的状态机判定。放在两处判会让
// 两边迟早不一致，而这类不一致的表现是"前端以为能点、点了报 409"。
var opsActions = map[string]string{
	"pause":  store.OpsPaused,
	"resume": store.OpsRunning,
	"cancel": store.OpsCanceled,
	"retry":  store.OpsPending,
}

// opsActionNotices 是每个动作成功后要说清楚的一句话。
//
// 尤其是 cancel：用户最需要知道的是"已经发出去的云端操作不会因此回滚"，
// 而不是"操作成功"。
var opsActionNotices = map[string]string{
	"pause":  "已暂停。恢复后会从停下的位置继续。",
	"resume": "已恢复，下一轮会立刻开始。",
	"cancel": "已取消。已经发往 Oracle 的请求不会被回滚，云端已产生的变更请到对应页面核对。",
	"retry":  "已重新排队。会从上次停下的阶段继续，不会从头重做。",
}

func (s *Server) handleOpsTaskAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.PathValue("action")

	to, ok := opsActions[action]
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_action",
			"只支持 pause / resume / cancel / retry")
		return
	}

	task, err := s.st.TransitionOpsTask(r.Context(), id, to)
	switch {
	case errors.Is(err, store.ErrOpsTaskNotFound):
		writeError(w, http.StatusNotFound, "not_found", "任务不存在")
		return
	case errors.Is(err, store.ErrOpsTaskIllegalTransition):
		// 409 而不是 400：请求本身没写错，是任务当前的状态不允许。
		// 前端据此提示"请刷新后重试"，而不是让用户去改参数。
		writeError(w, http.StatusConflict, "illegal_transition",
			"任务当前状态不允许该操作。"+err.Error())
		return
	case errors.Is(err, store.ErrOpsTaskConflict):
		writeError(w, http.StatusConflict, "state_changed",
			"任务状态刚刚被其他操作改变了，请刷新后重试")
		return
	case err != nil:
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "task_" + action, AccountID: task.AccountID,
		Target: task.Name, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"task":   toOpsDTO(*task, s.accountAliases(r.Context())),
		"notice": opsActionNotices[action],
	})
}

func (s *Server) handleDeleteOpsTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// 先读一次拿名字与账号：删完就查不到了，审计会缺关键信息。
	task, err := s.st.GetOpsTask(r.Context(), id)
	if errors.Is(err, store.ErrOpsTaskNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "任务不存在")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}

	if err := s.st.DeleteOpsTask(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrOpsTaskIllegalTransition) {
			writeError(w, http.StatusConflict, "illegal_transition", err.Error())
			return
		}
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "task_delete", AccountID: task.AccountID,
		Target: task.Name, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"notice": "记录已删除。云端已经发生的变更不受影响。",
	})
}
