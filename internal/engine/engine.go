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

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/connector"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/sink"
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
//
// 两个循环内部都检查 ctx：停止服务时不应把当前批（最多 BatchLimit 条，
// 每条又可能下载多个附件）全部处理完才退出——实测一批 88 条含附件时
// Ctrl+C 后要等十几分钟才响应。
func (e *Engine) runConnector(ctx context.Context, j ConnectorJob) {
	name := j.Conn.Name()
	for batch := 0; batch < j.MaxBatchesPerTick; batch++ {
		if ctx.Err() != nil {
			return
		}
		cp := e.getCheckpoint(name)
		records, next, err := j.Conn.Fetch(ctx, cp, j.BatchLimit)
		if err != nil {
			if ctx.Err() != nil {
				return // 取消导致的错误，不必刷日志
			}
			log.Printf("[%s] Fetch 失败: %v（下轮重试，游标不动）", name, err)
			return
		}
		if len(records) == 0 {
			log.Printf("[%s] 无新数据（游标 %s）", name, cp)
			return // 没有新数据
		}

			ok := 0
			interrupted := false

			// 开启批次缓冲模式（若 Sink 支持，避免高频单条读写打崩群晖 WebAPI）
			for _, s := range e.sinks {
				if b, ok := s.(sink.Batcher); ok {
					b.SetBatchMode(true)
				}
			}

			for i := range records {
				if ctx.Err() != nil {
					interrupted = true
					break
				}
				if err := e.dispatch(ctx, j.Conn, &records[i]); err != nil {
					if ctx.Err() != nil {
						interrupted = true
						break
					}
					// 写入失败的记录不 Ack：无游标连接器（如飞书）下一轮会重新扫描到它。
					log.Printf("[%s] 记录 %s 写入失败: %v（跳过该条）", name, records[i].ID, err)
					continue
				}
				// 写入成功后才确认：实现 AckConnector 的连接器在此刻持久化变更检测状态，
				// 保证「状态已记录」等价于「数据已落地」。
				if ack, ok := j.Conn.(connector.AckConnector); ok {
					if err := ack.Ack(records[i].ID); err != nil {
						log.Printf("[%s] 记录 %s 确认失败: %v", name, records[i].ID, err)
					}
				}
				ok++
			}

			// 整批分发后，统一刷写所有 Sink 的批次缓冲到存储后端
			flushCtx := ctx
			if interrupted {
				var cancel context.CancelFunc
				flushCtx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
			}
			for _, s := range e.sinks {
				if f, ok := s.(sink.Flusher); ok {
					if err := f.Flush(flushCtx); err != nil {
						log.Printf("[%s] 刷写 Sink %s 出错: %v", name, s.Name(), err)
					}
				}
				if b, ok := s.(sink.Batcher); ok {
					b.SetBatchMode(false)
				}
			}

			if interrupted {
				// 中断时不推进游标：未处理的记录下轮会重拉（NAS 写入与消息去重都是幂等的）
				log.Printf("[%s] 收到停止信号，本批已写入 %d/%d 条，游标保持 %s",
					name, ok, len(records), cp)
				return
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
