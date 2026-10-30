package logstream

import (
	"fmt"
	"testing"
	"time"
)

func TestRingWrapsAndKeepsOrder(t *testing.T) {
	r := NewRing(3)
	for i := 1; i <= 5; i++ {
		r.Push(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	got := r.Snapshot(0)
	if len(got) != 3 {
		t.Fatalf("条数 = %d，期望 3", len(got))
	}
	// 写满后保留的是最后 3 条，且按时间正序。
	for i, want := range []string{"m3", "m4", "m5"} {
		if got[i].Msg != want {
			t.Errorf("第 %d 条 = %q，期望 %q", i, got[i].Msg, want)
		}
	}
	if r.Len() != 3 {
		t.Errorf("Len = %d", r.Len())
	}
}

func TestRingSnapshotLimitTakesNewest(t *testing.T) {
	r := NewRing(10)
	for i := 1; i <= 5; i++ {
		r.Push(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	got := r.Snapshot(2)
	if len(got) != 2 || got[0].Msg != "m4" || got[1].Msg != "m5" {
		t.Fatalf("期望最后两条 m4/m5，实际 %+v", got)
	}
}

func TestRingEmptySnapshot(t *testing.T) {
	r := NewRing(4)
	if got := r.Snapshot(10); got != nil {
		t.Fatalf("空缓冲应返回 nil，实际 %+v", got)
	}
}

// 序号单调递增：SSE 的「先订阅再快照」靠它去重。
func TestRingSeqMonotonic(t *testing.T) {
	r := NewRing(2)
	for i := 0; i < 5; i++ {
		r.Push(Entry{Msg: "x"})
	}
	got := r.Snapshot(0)
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("序号没有递增: %d, %d", got[i-1].Seq, got[i].Seq)
		}
	}
}

func TestRingSubscribeReceives(t *testing.T) {
	r := NewRing(10)
	ch, cancel := r.Subscribe()
	defer cancel()

	r.Push(Entry{Msg: "hello"})
	select {
	case e := <-ch:
		if e.Msg != "hello" {
			t.Errorf("Msg = %q", e.Msg)
		}
	case <-time.After(time.Second):
		t.Fatal("订阅者没收到")
	}
}

// 卡住的订阅者绝不能阻塞写日志。
//
// 让一条挂掉的 SSE 连接把整个进程的日志堵死，是最糟的失败方式——
// 出问题时日志恰好是最需要的。
func TestRingSlowSubscriberDoesNotBlock(t *testing.T) {
	r := NewRing(10)
	_, cancel := r.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			r.Push(Entry{Msg: "x"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者没读，写入被堵住了")
	}
	if r.Dropped() == 0 {
		t.Error("丢弃计数应大于 0")
	}
}

// 取消订阅后继续写不能 panic（send on closed channel）。
func TestRingPushAfterCancelIsSafe(t *testing.T) {
	r := NewRing(10)
	_, cancel := r.Subscribe()
	cancel()
	cancel() // 幂等
	for i := 0; i < 10; i++ {
		r.Push(Entry{Msg: "x"})
	}
}

func TestRingCancelClosesChannel(t *testing.T) {
	r := NewRing(4)
	ch, cancel := r.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("取消后 channel 应已关闭")
	}
}

// nil Ring 要能用：日志组件没接线时（例如测试里）不该 panic。
func TestNilRingIsSafe(t *testing.T) {
	var r *Ring
	r.Push(Entry{Msg: "x"})
	if got := r.Snapshot(1); got != nil {
		t.Errorf("Snapshot = %+v", got)
	}
	if r.Len() != 0 || r.Dropped() != 0 {
		t.Error("nil Ring 的计数应为 0")
	}
	ch, cancel := r.Subscribe()
	defer cancel()
	if _, ok := <-ch; ok {
		t.Error("nil Ring 的 channel 应已关闭")
	}
}
