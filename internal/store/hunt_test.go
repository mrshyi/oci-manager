package store

import (
	"context"
	"testing"
	"time"
)

// TestUnixToTimeRoundTripsZero 锁住时间列的零值对称性。
//
// 约定是"0 表示未设置"，所以零值 Time 存进去是 0、读回来必须还是零值。
// 只要有一边不对称，所有 IsZero() 判断就全部失效——而失效的方向是"看起来
// 有值"，不会报错。
func TestUnixToTimeRoundTripsZero(t *testing.T) {
	if got := timeToUnix(time.Time{}); got != 0 {
		t.Fatalf("零值 Time 存成 %d，期望 0", got)
	}
	if got := unixToTime(0); !got.IsZero() {
		t.Fatalf("0 读回来是 %v，期望零值 Time——不对称会让 IsZero() 永远为假", got)
	}
	if got := unixToTime(1_700_000_000); got.Unix() != 1_700_000_000 {
		t.Fatalf("正常时间戳读回来是 %v", got)
	}
}

// TestHuntTaskWithoutExpirySurvivesRoundTrip 覆盖一个真实的线上症状。
//
// 不设到期时间的守候任务（ExpiresAt 零值）存进 hunt_tasks 再读出来，
// 曾经变成 1970-01-01；于是 shouldStop 判定"任务已到期，自动停止"，
// 任务刚创建就被判死。错误文案看起来完全合理，没人会怀疑是序列化问题。
func TestHuntTaskWithoutExpirySurvivesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	acc, err := st.CreateAccount(ctx, testAccount(t, "无到期测试", "noe", "aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateHuntTask(ctx, HuntTask{
		AccountID:       acc.ID,
		Region:          "ap-tokyo-1",
		Name:            "不限时",
		Spec:            "{}",
		ADs:             "AD-1",
		IntervalSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.GetHuntTask(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("读回来的到期时间是 %v，期望零值（表示不设到期）", got.ExpiresAt)
	}
	if got.MaxAttempts != 0 {
		t.Fatalf("MaxAttempts = %d，期望 0（表示不限次数）", got.MaxAttempts)
	}
}

// TestHuntTaskKeepsExplicitExpiry 确认设了到期时间的任务不被上面的修复影响。
func TestHuntTaskKeepsExplicitExpiry(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	acc, err := st.CreateAccount(ctx, testAccount(t, "到期测试", "exp", "bbbb"))
	if err != nil {
		t.Fatal(err)
	}
	// 存到秒：数据库列是 INTEGER Unix 秒，纳秒会被截掉。
	expires := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	created, err := st.CreateHuntTask(ctx, HuntTask{
		AccountID:       acc.ID,
		Region:          "ap-tokyo-1",
		Name:            "72 小时",
		Spec:            "{}",
		ADs:             "AD-1",
		IntervalSeconds: 60,
		MaxAttempts:     100,
		ExpiresAt:       expires,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.GetHuntTask(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Fatalf("到期时间读回来是 %v，期望 %v", got.ExpiresAt, expires)
	}
	if got.MaxAttempts != 100 {
		t.Fatalf("MaxAttempts = %d，期望 100", got.MaxAttempts)
	}
}

// TestDueHuntTasksSkipsPaused 确认暂停的任务不会被捞到。
//
// 这是 P1-1 状态机验收里的一条，抢机这边先钉住：paused 只存在于查询条件里，
// 漏了的话"暂停"按钮点了没用，而界面上的状态是 paused——看起来完全正常。
func TestDueHuntTasksSkipsPaused(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	acc, err := st.CreateAccount(ctx, testAccount(t, "暂停测试", "pau", "cccc"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateHuntTask(ctx, HuntTask{
		AccountID:       acc.ID,
		Region:          "ap-tokyo-1",
		Name:            "要暂停的",
		Spec:            "{}",
		ADs:             "AD-1",
		IntervalSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	due, err := st.DueHuntTasks(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("运行中的任务应当被捞到，实际 %d 条", len(due))
	}

	if _, err := st.SetHuntState(ctx, task.ID, HuntPaused); err != nil {
		t.Fatal(err)
	}
	due, err = st.DueHuntTasks(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("暂停之后仍有 %d 条被捞到", len(due))
	}
}
