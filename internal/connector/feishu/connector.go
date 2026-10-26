// 飞书云文档连接器：tenant（共享文件夹遍历）+ user（已授权用户云空间）双模式。
// 变更检测：feishu_doc_state(token → edit_time)，只产出变化的文档。
// 归属：文档 owner + 可见的已授权用户 都进 OwnerIDs，NAS sink fan-out 到各自目录（含"归属别人但我可见"副本）。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"time"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// Config 飞书连接器配置。
type Config struct {
	AppID     string   `yaml:"app_id"`
	AppSecret string   `yaml:"app_secret"`
	Mode      string   `yaml:"mode"`    // tenant / user / both
	Folders   []string `yaml:"folders"` // tenant 模式遍历的共享文件夹 token 列表
	// WikiSpaceIDs 额外遍历的知识库空间 ID。用于「我的文档库」这类
	// wiki/v2/spaces 不返回、但按 ID 可以正常列节点的空间。
	WikiSpaceIDs []string `yaml:"wiki_space_ids"`
}

// API 抽象（便于测试替换；feishuapi.Client 适配实现）。
type API interface {
	ListFolderChildren(token, folderToken string) ([]feishuapi.DriveFile, error)
	WalkFolder(token, folderToken string, depth int, fn func(feishuapi.DriveFile)) error
	RootFolderMeta(token string) (string, error)
	DownloadFile(token, fileToken string) (io.ReadCloser, error)
	DocxRawContent(token, documentID string) (string, error)
	DocRawContent(token, docToken string) (string, error)
	TenantAccessToken() (string, error)

	// 知识库（wiki）：飞书新版「我的文档库」只能从这里列出来
	ListWikiSpaces(token string) ([]feishuapi.WikiSpace, error)
	WalkWikiSpace(token, spaceID string, maxDepth int, fn func(feishuapi.WikiSpace, feishuapi.WikiNode)) error

	// 导出云文档源文件（docx/sheet/bitable → .docx/.xlsx）。
	// raw_content 只有纯文本会丢图片与样式，导出才能拿到完整源文件。
	ExportFile(tok, token, docType string) (string, error)
	DownloadExport(tok, fileToken string) (io.ReadCloser, error)
}

// 限流：drive 下载约 5 QPS，FetchAttachment 前统一减速。
const downloadMinInterval = 220 * time.Millisecond

// Connector 飞书云文档连接器。
type Connector struct {
	cfg   Config
	api   API
	store *storage.Store

	tenantToken string
	pending     []model.Record        // 本轮已扫描出的待产出行列（增量）
	pendingMeta map[string]pendingDoc // 待 Ack 的文档元数据（id → 状态）
	lastDl      atomic.Int64          // 上次下载时间（纳秒），限流用
}

func New(cfg Config, store *storage.Store) *Connector {
	return &Connector{
		cfg:         cfg,
		store:       store,
		pendingMeta: map[string]pendingDoc{},
	}
}

// NewWithAPI 注入自定义 API（测试用）。
func NewWithAPI(cfg Config, store *storage.Store, api API) *Connector {
	return &Connector{
		cfg:         cfg,
		store:       store,
		api:         api,
		pendingMeta: map[string]pendingDoc{},
	}
}

func (c *Connector) Name() string { return "feishu" }

func (c *Connector) Init(ctx context.Context) error {
	if c.api == nil {
		c.api = feishuapi.New(c.cfg.AppID, c.cfg.AppSecret)
	}
	if c.mode() != "user" {
		tat, err := c.api.TenantAccessToken()
		if err != nil {
			return fmt.Errorf("tenant_access_token: %w", err)
		}
		c.tenantToken = tat
	}
	return nil
}

func (c *Connector) mode() string {
	if c.cfg.Mode == "" {
		return "tenant"
	}
	return c.cfg.Mode
}

// docToRecord 文档 → 统一 Record。owner + viewer 都进 OwnerIDs（各存一份）。
func docToRecord(f feishuapi.DriveFile, viewerID string) model.Record {
	owners := []string{}
	if f.OwnerID != "" {
		owners = append(owners, f.OwnerID)
	}
	if viewerID != "" && viewerID != f.OwnerID {
		owners = append(owners, viewerID)
	}
	meta, _ := json.Marshal(map[string]any{
		"token": f.Token, "name": f.Name, "type": f.Type,
		"owner_id": f.OwnerID, "url": f.URL, "modified_time": f.ModifiedTime,
	})
	return model.Record{
		Source:    "feishu",
		Kind:      f.Type,
		ID:        f.Token,
		Timestamp: feishuapi.UnixSeconds(f.ModifiedTime) * 1000,
		OwnerIDs:  owners,
		SenderID:  f.OwnerID, // 归属以所有者为主
		Body:      meta,
		Attachments: []model.Attachment{{
			Name:   attachmentName(f),
			FileID: attachmentFileID(f),
		}},
	}
}

// attachmentFileID 附件标识：file 类型走 drive 下载；docx/doc 走 raw_content。
func attachmentFileID(f feishuapi.DriveFile) string {
	switch f.Type {
	case "file":
		return "file:" + f.Token
	case "docx":
		return "docx:" + f.Token
	case "doc":
		return "doc:" + f.Token
	}
	return f.Type + ":" + f.Token
}

// attachmentName 附件名（文档补 .md 后缀；上传类文件用原名）。
// attachmentName 归档文件名。
// 按「导出后的源文件」确定后缀，而不是原节点类型：
//   docx/doc  → .docx   （导出接口产出 Word 文件，不是纯文本）
//   sheet     → .xlsx   （导出接口产出 Excel 文件）
//   bitable   → .xlsx
//   file      → 保留原文件名（用户上传的文件，一字不改）
func attachmentName(f feishuapi.DriveFile) string {
	name := f.Name
	if name == "" {
		name = f.Token
	}
	// 去掉可能已存在的后缀，统一按导出格式重加
	base := name
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	switch f.Type {
	case "docx", "doc":
		if !strings.HasSuffix(strings.ToLower(name), ".docx") {
			name = base + ".docx"
		}
	case "sheet", "bitable":
		if !strings.HasSuffix(strings.ToLower(name), ".xlsx") {
			name = base + ".xlsx"
		}
	}
	return name
}

// Fetch 产出变化的文档（pending 为空时先全量扫描一遍）。
func (c *Connector) Fetch(ctx context.Context, checkpoint string, limit int) ([]model.Record, string, error) {
	if len(c.pending) == 0 {
		if err := c.scan(ctx); err != nil {
			return nil, checkpoint, err
		}
	}
	n := limit
	if n <= 0 || n > len(c.pending) {
		n = len(c.pending)
	}
	batch := c.pending[:n]
	c.pending = c.pending[n:]
	next := "done"
	if len(c.pending) > 0 {
		next = "cont"
	}
	log.Printf("[feishu] 本批产出 %d 篇文档（剩余待产出 %d）", len(batch), len(c.pending))
	return batch, next, nil
}

// scan 全量扫描（tenant 共享文件夹 + 各已授权用户云空间），筛出变化的文档进 pending。
func (c *Connector) scan(ctx context.Context) error {
	mode := c.mode()
	seen := map[string]bool{}

	// tenant 模式：遍历配置的共享文件夹
	if (mode == "tenant" || mode == "both") && c.tenantToken != "" {
		for _, folder := range c.cfg.Folders {
			err := c.api.WalkFolder(c.tenantToken, folder, 10, func(f feishuapi.DriveFile) {
				c.collect(docToRecord(f, ""), seen)
			})
			if err != nil {
				log.Printf("[feishu] 遍历共享文件夹 %s 失败: %v", folder, err)
			}
		}
	}

	// user 模式：每个已授权用户
	if mode == "user" || mode == "both" {
		grants, err := c.store.ListOAuthGrants("feishu")
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, g := range grants {
			if g.Status != "authorized" || g.AccessToken == "" || g.ExpireAt <= now {
				log.Printf("[feishu] 用户 %s 授权无效（status=%s），跳过", g.ExternalUserID, g.Status)
				continue
			}
			// a) 云空间（drive）：老式文件夹结构。
			// 注意：新版飞书把「归我所有/与我共享」也归到这里，但要求 token 带
			// space:document:retrieve；只有 drive:drive 时接口 code=0 却返回空列表，
			// 非常容易被误判成「用户没有文档」。数量为 0 时打出提示。
			root, err := c.api.RootFolderMeta(g.AccessToken)
			if err != nil {
				log.Printf("[feishu] 拿 %s 根目录失败: %v", g.ExternalUserID, err)
			} else {
				driveN := 0
				err = c.api.WalkFolder(g.AccessToken, root, 10, func(f feishuapi.DriveFile) {
					driveN++
					c.collect(docToRecord(f, g.ExternalUserID), seen)
				})
				if err != nil {
					log.Printf("[feishu] 遍历 %s 云空间失败: %v", g.ExternalUserID, err)
				} else if driveN == 0 {
					// 空列表有两种可能：真没文档，或 token 缺 space:document:retrieve
					// （后者接口仍是 code=0，只是不返回数据）。提示重新授权排查。
					log.Printf("[feishu] %s 云空间：0 个条目（若确认有文档，检查 token 是否含 "+
						"space:document:retrieve，缺该 scope 时接口不报错但返回空）", g.ExternalUserID)
				} else {
					log.Printf("[feishu] %s 云空间：%d 个条目", g.ExternalUserID, driveN)
				}
			}

			// b) 团队知识库：wiki/v2/spaces 返回的空间
			spaceIDs := append([]string{}, c.cfg.WikiSpaceIDs...)
			spaces, err := c.api.ListWikiSpaces(g.AccessToken)
			if err != nil {
				// 缺 wiki:wiki:readonly 时这里会报 99991679，提示需要重新授权
				log.Printf("[feishu] 列 %s 知识库失败（可能缺 wiki:wiki:readonly，需重新授权）: %v",
					g.ExternalUserID, err)
			} else {
				for _, sp := range spaces {
					spaceIDs = append(spaceIDs, sp.SpaceID)
				}
			}

			// ListWikiSpaces 不返回「我的文档库」（官方明确说明），
			// 这类空间只能通过配置 wiki_space_ids 显式指定，再按 ID 列节点。
			total := 0
			seenSpace := map[string]bool{}
			for _, sid := range spaceIDs {
				if sid == "" || seenSpace[sid] {
					continue
				}
				seenSpace[sid] = true
				n := 0
				err := c.api.WalkWikiSpace(g.AccessToken, sid, 10, func(_ feishuapi.WikiSpace, node feishuapi.WikiNode) {
					if rec, ok := wikiNodeToRecord(node, g.ExternalUserID); ok {
						n++
						c.collect(rec, seen)
					}
				})
				if err != nil {
					log.Printf("[feishu] 遍历知识库 %s 失败: %v", sid, err)
					continue
				}
				total += n
				if n > 0 {
					log.Printf("[feishu] 知识库 %s：%d 篇", sid, n)
				}
			}
			log.Printf("[feishu] %s 知识库：%d 个空间，%d 篇文档", g.ExternalUserID, len(seenSpace), total)
		}
	}
	log.Printf("[feishu] 扫描完成，待产出 %d 篇", len(c.pending))
	return nil
}

// wikiNodeToRecord wiki 节点 → Record。
//
// 支持的源类型：
//   file  上传的文件 → 直接下载原始字节（无损）
//   docx  新版文档   → 导出为 .docx（保留格式与内嵌图片，优于纯文本）
//   doc   旧版文档   → 导出为 .docx
//   sheet 电子表格   → 导出为 .xlsx
//   bitable 多维表格 → 导出为 .xlsx
//
// 原则：一律取「源文件」而非转换后的文本，避免丢内容。
func wikiNodeToRecord(n feishuapi.WikiNode, viewerID string) (model.Record, bool) {
	switch n.ObjType {
	case "file", "docx", "doc", "sheet", "bitable":
	default:
		return model.Record{}, false
	}
	token := n.ObjToken
	if token == "" {
		return model.Record{}, false
	}
	f := feishuapi.DriveFile{
		Token:        token,
		Name:         n.Title,
		Type:         n.ObjType,
		ModifiedTime: n.NodeEditTime,
	}
	if f.ModifiedTime == "" {
		f.ModifiedTime = n.NodeCreateTime
	}
	// wiki 节点不返回文档 owner，用授权人自己作为归属人：
	// 这批文档是通过他的授权才可见的，归到他名下符合业务预期。
	if viewerID != "" {
		f.OwnerID = viewerID
	}
	return docToRecord(f, viewerID), true
}

// collect 变更检测 + 入队（同 token 只入一次）。
// 入参是已构造好的 Record，drive 与 wiki 两条来源共用同一套去重与台账逻辑。
func (c *Connector) collect(rec model.Record, seen map[string]bool) {
	if seen[rec.ID] {
		return
	}
	seen[rec.ID] = true
	editTime := rec.Timestamp / 1000
	if editTime > 0 && editTime <= c.store.GetDocEditTime(rec.ID) {
		return // 未变化
	}
	c.pending = append(c.pending, rec)

	// 这里只入队，不写 doc_state。
	// 状态必须等 Sink 全部写入成功后再由 Ack 记录，否则写 NAS 失败时
	// 下一轮会被判为「未变化」而永久跳过——文档静默丢失。
	// 待确认的记录暂存在 pendingMeta，供 Ack 取用元数据。
	c.pendingMeta[rec.ID] = pendingDoc{
		title:    attachmentNameOf(rec),
		ownerID:  rec.SenderID,
		editTime: editTime,
	}
}

// pendingDoc 待确认文档的元数据（Ack 时写 doc_state 用）。
type pendingDoc struct {
	title    string
	ownerID  string
	editTime int64
}

// Ack 实现 connector.AckConnector：Sink 写入成功后记录变更检测状态。
func (c *Connector) Ack(recordID string) error {
	meta, ok := c.pendingMeta[recordID]
	if !ok {
		return nil // 非本连接器产出的记录，忽略
	}
	delete(c.pendingMeta, recordID)
	return c.store.UpsertDocState(recordID, meta.title, meta.ownerID, meta.editTime)
}

// attachmentNameOf 取 Record 里第一个附件的名字（写 doc_state 用）。
func attachmentNameOf(rec model.Record) string {
	if len(rec.Attachments) > 0 {
		return rec.Attachments[0].Name
	}
	return ""
}

// FetchAttachment 拉取文档内容（file 原始字节 / docx·doc 纯文本），带限流。
// FetchAttachment 取云文档的「源文件」。
//
//   file            直接下载原始字节（上传的文件，无损）
//   docx/doc        导出为 .docx（保留图片、表格、样式；raw_content 只有纯文本会丢内容）
//   sheet/bitable   导出为 .xlsx
//
// 统一用 user token：导出接口要求调用者对文档有阅读权限，
// tenant token 需要在每篇文档上单独「添加文档应用」，运维成本过高。
func (c *Connector) FetchAttachment(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
	c.throttle()
	scheme, token, _ := strings.Cut(a.FileID, ":")
	tok := c.userToken()
	if tok == "" {
		tok = c.tenantToken // 兜底：没有用户授权时用应用身份
	}

	switch scheme {
	case "file":
		return c.api.DownloadFile(tok, token)
	case "docx", "doc", "sheet", "bitable":
		// 导出是异步的（创建→轮询→下载），期间要响应取消
		type res struct {
			rc  io.ReadCloser
			err error
		}
		ch := make(chan res, 1)
		go func() {
			fileToken, err := c.api.ExportFile(tok, token, scheme)
			if err != nil {
				ch <- res{nil, err}
				return
			}
			rc, err := c.api.DownloadExport(tok, fileToken)
			ch <- res{rc, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.rc, r.err
		}
	}
	return nil, fmt.Errorf("暂不支持的文档类型: %s", scheme)
}

// userToken 取当前可用的用户 token（导出等接口需要用户身份）。
func (c *Connector) userToken() string {
	grants, err := c.store.ListOAuthGrants("feishu")
	if err != nil || len(grants) == 0 {
		return ""
	}
	now := time.Now().Unix()
	for _, g := range grants {
		if g.Status == "authorized" && g.AccessToken != "" && g.ExpireAt > now {
			return g.AccessToken
		}
	}
	return ""
}

// throttle 下载限速（≥220ms 间隔 ≈ 4.5 QPS）。
func (c *Connector) throttle() {
	now := time.Now().UnixNano()
	last := c.lastDl.Load()
	if wait := downloadMinInterval - time.Duration(now-last); wait > 0 && last > 0 {
		time.Sleep(wait)
	}
	c.lastDl.Store(time.Now().UnixNano())
}

func (c *Connector) Close() error { return nil }
