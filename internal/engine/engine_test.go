package engine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/sink"
)

// fakeConn 假连接器：cp=="" 返回 a/b 两条，next="2"；cp=="2" 返回 second 里配置的记录（默认空）。
type fakeConn struct {
	second []model.Record
}

func (c *fakeConn) Name() string                   { return "fake" }
func (c *fakeConn) Init(ctx context.Context) error { return nil }
func (c *fakeConn) Close() error                   { return nil }
func (c *fakeConn) FetchAttachment(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
	return nil, nil
}

func (c *fakeConn) Fetch(ctx context.Context, checkpoint string, limit int) ([]model.Record, string, error) {
	switch checkpoint {
	case "":
		return []model.Record{
			{Source: "fake", Kind: "text", ID: "a"},
			{Source: "fake", Kind: "text", ID: "b"},
		}, "2", nil
	case "2":
		if len(c.second) == 0 {
			return nil, "3", nil
		}
		return c.second, "3", nil
	}
	return nil, checkpoint, nil
}

// fakeSink 假 Sink：failIDs 里的记录写入失败，并统计每条记录的写入次数。
type fakeSink struct {
	failIDs map[string]bool
	calls   map[string]int
}

func newFakeSink() *fakeSink {
	return &fakeSink{failIDs: map[string]bool{}, calls: map[string]int{}}
}

func (s *fakeSink) Name() string { return "fake" }

func (s *fakeSink) WriteRecord(ctx context.Context, r model.Record, read sink.AttachmentReader) error {
	s.calls[r.ID]++
	if s.failIDs[r.ID] {
		return errors.New("模拟写入失败")
	}
	return nil
}

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "state.json"))
}

// addJob 走 AddConnector 注册（同时验证默认值填充：BatchLimit/MaxBatchesPerTick/MaxFailedRetries）。
func (e *Engine) addJob(j ConnectorJob) ConnectorJob {
	e.AddConnector(j)
	return e.jobs[len(e.jobs)-1]
}

// 连续失败 MaxFailedRetries 轮后转死信并推进游标。
func TestHoldCursorRetriesThenDeadLetters(t *testing.T) {
	e := newTestEngine(t)
	conn := &fakeConn{}
	sk := newFakeSink()
	sk.failIDs["b"] = true
	e.AddSink(sk)

	var deadLettered []string
	job := e.addJob(ConnectorJob{
		Conn:                conn,
		BatchLimit:          10,
		HoldCursorOnFailure: true,
		MaxFailedRetries:    3,
		DeadLetter: func(r model.Record, err error) error {
			deadLettered = append(deadLettered, r.ID)
			return nil
		},
	})
	ctx := context.Background()

	// 第 1、2 次失败：游标保持 ""
	for i := 1; i <= 2; i++ {
		e.runConnector(ctx, job)
		if cp := e.getCheckpoint("fake"); cp != "" {
			t.Fatalf("第 %d 轮后游标应保持空: %q", i, cp)
		}
		if len(deadLettered) != 0 {
			t.Fatalf("第 %d 轮后不应写死信: %v", i, deadLettered)
		}
	}

	// 第 3 次失败：重试耗尽，转死信并推进游标
	e.runConnector(ctx, job)
	if len(deadLettered) != 1 || deadLettered[0] != "b" {
		t.Fatalf("死信应只收到 b 一次: %v", deadLettered)
	}
	if cp := e.getCheckpoint("fake"); cp != "2" {
		t.Fatalf("死信写入成功后游标应为 2: %q", cp)
	}
}

// 失败一轮后恢复：游标推进且连续失败计数清零。
func TestHoldCursorRecovers(t *testing.T) {
	e := newTestEngine(t)
	conn := &fakeConn{}
	sk := newFakeSink()
	sk.failIDs["b"] = true
	e.AddSink(sk)

	deadLetterCalls := 0
	job := e.addJob(ConnectorJob{
		Conn:                conn,
		BatchLimit:          10,
		HoldCursorOnFailure: true,
		MaxFailedRetries:    2,
		DeadLetter: func(r model.Record, err error) error {
			deadLetterCalls++
			return nil
		},
	})
	ctx := context.Background()

	// 第 1 轮：b 失败，游标保持
	e.runConnector(ctx, job)
	if cp := e.getCheckpoint("fake"); cp != "" {
		t.Fatalf("失败轮游标应保持空: %q", cp)
	}

	// 第 2 轮：b 成功，游标推进到 2，死信未被调用
	sk.failIDs = map[string]bool{}
	e.runConnector(ctx, job)
	if cp := e.getCheckpoint("fake"); cp != "2" {
		t.Fatalf("恢复后游标应为 2: %q", cp)
	}
	if deadLetterCalls != 0 {
		t.Fatalf("恢复轮不应写死信: %d", deadLetterCalls)
	}
	if e.failStreak["fake"] != 0 {
		t.Fatalf("游标推进后失败计数应清零: %d", e.failStreak["fake"])
	}

	// 第 3 轮：游标 2 处 c 失败，streak 必须从 1 重新计（MaxFailedRetries=2 时不转死信）
	conn.second = []model.Record{{Source: "fake", Kind: "text", ID: "c"}}
	sk.failIDs["c"] = true
	e.runConnector(ctx, job)
	if cp := e.getCheckpoint("fake"); cp != "2" {
		t.Fatalf("新失败轮游标应保持在 2: %q", cp)
	}
	if e.failStreak["fake"] != 1 {
		t.Fatalf("连续失败计数应从 1 重新开始: %d", e.failStreak["fake"])
	}
	if deadLetterCalls != 0 {
		t.Fatalf("计数已清零时第 1 次失败不应写死信: %d", deadLetterCalls)
	}
}

// 未配置 DeadLetter：重试耗尽也不推进游标（宁可卡住也不丢）。
func TestHoldCursorNoDeadLetterKeepsCursor(t *testing.T) {
	e := newTestEngine(t)
	conn := &fakeConn{}
	sk := newFakeSink()
	sk.failIDs["b"] = true
	e.AddSink(sk)

	job := e.addJob(ConnectorJob{
		Conn:                conn,
		BatchLimit:          10,
		HoldCursorOnFailure: true,
		MaxFailedRetries:    2,
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		e.runConnector(ctx, job)
		if cp := e.getCheckpoint("fake"); cp != "" {
			t.Fatalf("第 %d 轮后游标应保持空: %q", i+1, cp)
		}
	}
}

// 默认策略（飞书等无游标连接器）保持不变：失败条跳过，游标照常推进。
func TestDefaultSkipBehaviorUnchanged(t *testing.T) {
	e := newTestEngine(t)
	conn := &fakeConn{}
	sk := newFakeSink()
	sk.failIDs["b"] = true
	e.AddSink(sk)

	job := e.addJob(ConnectorJob{
		Conn:       conn,
		BatchLimit: 10,
	})
	ctx := context.Background()
	e.runConnector(ctx, job)
	if cp := e.getCheckpoint("fake"); cp != "2" {
		t.Fatalf("默认策略下游标应推进到 2: %q", cp)
	}
	if sk.calls["a"] != 1 || sk.calls["b"] != 1 {
		t.Fatalf("两条记录都应尝试写入一次: %+v", sk.calls)
	}
}

// 死信写入失败：游标保持不动，等下一轮继续。
func TestDeadLetterErrorKeepsCursor(t *testing.T) {
	e := newTestEngine(t)
	conn := &fakeConn{}
	sk := newFakeSink()
	sk.failIDs["b"] = true
	e.AddSink(sk)

	job := e.addJob(ConnectorJob{
		Conn:                conn,
		BatchLimit:          10,
		HoldCursorOnFailure: true,
		MaxFailedRetries:    1,
		DeadLetter: func(r model.Record, err error) error {
			return errors.New("死信表写入失败")
		},
	})
	ctx := context.Background()
	e.runConnector(ctx, job)
	if cp := e.getCheckpoint("fake"); cp != "" {
		t.Fatalf("死信写入失败时游标应保持空: %q", cp)
	}
}
