// Package engine 核心引擎：调度连接器增量拉取，把 Record 管线式写入所有 Sink。
// 引擎不感知任何具体平台，只做四件事：调度、状态、管线、观测。
package engine

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"fastgpt-sync-adapter/adapter/internal/connector"
	"fastgpt-sync-adapter/adapter/internal/model"
	"fastgpt-sync-adapter/adapter/internal/sink"
)

// ConnectorJob 一个连接器的运行配置。
type ConnectorJob struct {
	Conn       connector.Connector
	Interval   time.Duration // 拉取间隔
	BatchLimit int           // 单次 Fetch 条数上限
	RunOnStart bool          // 启动即跑一轮
	// MaxBatchesPerTick 每轮最多连续 Fetch 的批数（防止历史数据量大时一轮跑太久），0=默认 50。
	MaxBatchesPerTick int
}

// Task 周期性任务（不产出 Record 的旁路工作，如 NAS→FastGPT 文件上传扫描）。
type Task interface {
	Name() string
	Interval() time.Duration
	Run(ctx context.Context) error
}

// Engine 核心引擎。
type Engine struct {
	statePath  string
	jobs       []ConnectorJob
	sinks      []sink.Sink
	tasks      []Task
	mu         sync.Mutex
	checkpoint map[string]string // connector name -> checkpoint
}

func New(statePath string) *Engine {
	return &Engine{statePath: statePath, checkpoint: map[string]string{}}
}

func (e *Engine) AddConnector(j ConnectorJob) {
	if j.BatchLimit <= 0 {
		j.BatchLimit = 100
	}
	if j.MaxBatchesPerTick <= 0 {
		j.MaxBatchesPerTick = 50
	}
	e.jobs = append(e.jobs, j)
}

func (e *Engine) AddSink(s sink.Sink)       { e.sinks = append(e.sinks, s) }
func (e *Engine) AddTask(t Task)            { e.tasks = append(e.tasks, t) }
func (e *Engine) Sinks() []sink.Sink        { return e.sinks }

// Run 启动所有连接器循环与任务循环，阻塞直到 ctx 取消。
func (e *Engine) Run(ctx context.Context) error {
	if err := e.loadState(); err != nil {
		log.Printf("[engine] 状态文件读取失败（将从零开始）: %v", err)
	}

	// 初始化连接器
	for _, j := range e.jobs {
		if err := j.Conn.Init(ctx); err != nil {
			return err
		}
		defer j.Conn.Close()
	}

	var wg sync.WaitGroup
	for _, j := range e.jobs {
		wg.Add(1)
		go func(j ConnectorJob) {
			defer wg.Done()
			e.connectorLoop(ctx, j)
		}(j)
	}
	for _, t := range e.tasks {
		wg.Add(1)
		go func(t Task) {
			defer wg.Done()
			e.taskLoop(ctx, t)
		}(t)
	}

	log.Printf("[engine] 已启动：%d 个连接器，%d 个 Sink，%d 个任务", len(e.jobs), len(e.sinks), len(e.tasks))
	wg.Wait()
	return nil
}

func (e *Engine) connectorLoop(ctx context.Context, j ConnectorJob) {
	name := j.Conn.Name()
	if j.RunOnStart {
		e.runConnector(ctx, j)
	}
	ticker := time.NewTicker(j.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[engine] 连接器 %s 停止", name)
			return
		case <-ticker.C:
			e.runConnector(ctx, j)
		}
	}
}

// runConnector 执行一轮：循环 Fetch（直到不足一批或达批数上限），逐条写入所有 Sink。
func (e *Engine) runConnector(ctx context.Context, j ConnectorJob) {
	name := j.Conn.Name()
	for batch := 0; batch < j.MaxBatchesPerTick; batch++ {
		cp := e.getCheckpoint(name)
		records, next, err := j.Conn.Fetch(ctx, cp, j.BatchLimit)
		if err != nil {
			log.Printf("[%s] Fetch 失败: %v（下轮重试，游标不动）", name, err)
			return
		}
		if len(records) == 0 {
			return // 没有新数据
		}

		ok := 0
		for i := range records {
			if err := e.dispatch(ctx, j.Conn, &records[i]); err != nil {
				log.Printf("[%s] 记录 %s 写入失败: %v（跳过该条）", name, records[i].ID, err)
				continue
			}
			ok++
		}
		// 整批处理完才推进游标（失败条已隔离，下轮不会重拉——企微侧以 seq 为准）
		e.setCheckpoint(name, next)
		log.Printf("[%s] 批次 %d：拉取 %d 条，写入成功 %d 条，游标 → %s", name, batch+1, len(records), ok, next)

		if len(records) < j.BatchLimit {
			return // 已拉到最新
		}
		// 满批说明可能还有，继续拉
	}
	log.Printf("[%s] 达到单轮批数上限（%d），剩余数据下轮继续", name, j.MaxBatchesPerTick)
}

// dispatch 把一条记录写入所有 Sink（任一 Sink 失败即视为该条失败，但各 Sink 独立尝试）。
func (e *Engine) dispatch(ctx context.Context, c connector.Connector, r *model.Record) error {
	read := func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
		return c.FetchAttachment(ctx, a)
	}
	var firstErr error
	for _, s := range e.sinks {
		if err := s.WriteRecord(ctx, *r, read); err != nil {
			log.Printf("[%s→%s] 写入失败 id=%s: %v", c.Name(), s.Name(), r.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (e *Engine) taskLoop(ctx context.Context, t Task) {
	ticker := time.NewTicker(t.Interval())
	defer ticker.Stop()
	// 任务启动即跑一轮
	if err := t.Run(ctx); err != nil {
		log.Printf("[task:%s] 运行出错: %v", t.Name(), err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := t.Run(ctx); err != nil {
				log.Printf("[task:%s] 运行出错: %v", t.Name(), err)
			}
		}
	}
}

// ===== checkpoint 状态持久化 =====

func (e *Engine) getCheckpoint(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checkpoint[name]
}

func (e *Engine) setCheckpoint(name, cp string) {
	e.mu.Lock()
	e.checkpoint[name] = cp
	data, _ := json.MarshalIndent(e.checkpoint, "", "  ")
	e.mu.Unlock()
	if err := os.WriteFile(e.statePath, data, 0644); err != nil {
		log.Printf("[engine] 状态写入失败 %s: %v", e.statePath, err)
	}
}

func (e *Engine) loadState() error {
	data, err := os.ReadFile(e.statePath)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return json.Unmarshal(data, &e.checkpoint)
}
