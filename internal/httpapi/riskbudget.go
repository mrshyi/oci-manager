package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"ocicore/internal/riskguard"
)

// 调用预算的只读视图。
//
// 存在的意义是「让降频有解释」。预算生效之后，用户会看到抢机任务从 60 秒
// 一次变成 240 秒一次，如果界面上没有任何地方说明原因，第一反应是「这工具
// 卡了」——然后去重启它，于是预算重新计数、间隔恢复，问题看起来"自己好了"，
// 直到下次又变慢。把已用/上限/倍数摆出来，慢就变成了一个可理解的状态。

// budgetItem 是单个账号的预算视图。
type budgetItem struct {
	AccountID string `json:"accountId"`
	Alias     string `json:"alias"`
	Used      int    `json:"used"`
	Limit     int    `json:"limit"`
	// Multiplier 是任务层当前的降频倍数：1 表示不降频。
	Multiplier int `json:"multiplier"`
}

// handleRiskBudget 返回本小时各账号的调用预算用量。
//
// 只读，且不触发任何 OCI 调用——它读的是本地计数。
func (s *Server) handleRiskBudget(w http.ResponseWriter, r *http.Request) {
	// 未接预算时明确返回 enabled=false，而不是一个空列表：
	// 空列表看起来像「所有账号都是 0 次」，会让人误以为统计在工作。
	if s.budget == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":  false,
			"window":   riskguard.Window.String(),
			"accounts": []budgetItem{},
		})
		return
	}

	accounts, err := s.st.ListAccounts(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}

	items := make([]budgetItem, 0, len(accounts))
	for _, acc := range accounts {
		used, limit := s.budget.Usage(r.Context(), acc.ID)
		items = append(items, budgetItem{
			AccountID:  acc.ID,
			Alias:      acc.Alias,
			Used:       used,
			Limit:      limit,
			Multiplier: s.budget.Multiplier(r.Context(), acc.ID),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     true,
		"window":      riskguard.Window.String(),
		"windowStart": riskguard.WindowStart(time.Now()),
		"limit":       s.budget.Limit(),
		"accounts":    items,
	})
}

// riskBudgetRetention 是调用预算记录的保留时长。
//
// 留 48 小时：够回答「昨天下午那阵为什么变慢了」，再久的窗口对排查
// 没有意义，而这张表每小时给每个账号长一行。
const riskBudgetRetention = 48 * time.Hour

// riskBudgetPruneInterval 是清理周期。数据量极小，一天一次足够。
const riskBudgetPruneInterval = 6 * time.Hour

// RunRiskBudgetPruner 定期清理过期的预算记录，直到 ctx 结束。
func (s *Server) RunRiskBudgetPruner(ctx context.Context) {
	ticker := time.NewTicker(riskBudgetPruneInterval)
	defer ticker.Stop()

	s.pruneRiskBudgetOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pruneRiskBudgetOnce(ctx)
		}
	}
}

func (s *Server) pruneRiskBudgetOnce(ctx context.Context) {
	// 按窗口起点比较，所以截止时间要先对齐到小时，否则会误删当前窗口
	// 里刚攒起来的计数——那等于每小时随机清零一次。
	cutoff := riskguard.WindowStart(time.Now().Add(-riskBudgetRetention))
	if _, err := s.st.PurgeRiskBudget(ctx, cutoff); err != nil {
		slog.Warn("清理调用预算记录失败", "err", err)
	}
}
