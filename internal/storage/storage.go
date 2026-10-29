// Package storage SQLite 存储层：授权、身份映射、文件推送台账、连接器游标。
// 用纯 Go 的 modernc.org/sqlite（不依赖 cgo，避免与企微 SDK 的 cgo 构建冲突，交叉编译也省事）。
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/apitoken"
)

// Store 存储入口。
type Store struct {
	db *sql.DB
}

// OAuthGrant 一个外部平台用户的授权记录（飞书 OAuth）。
type OAuthGrant struct {
	Platform        string // feishu
	ExternalUserID  string // 飞书 open_id / union_id
	Code            string // 授权 code（一次性，留存便于审计）
	AccessToken     string // user_access_token
	RefreshToken    string
	ExpireAt        int64  // access_token 过期时间（unix 秒）
	RefreshExpireAt int64  // refresh_token 过期时间（unix 秒）
	Status          string // authorized / expired / revoked
	UpdatedAt       int64
}

// Identity 企微 ↔ 飞书 ↔ 姓名 的身份映射（数据源：飞书多维表格，本地缓存）。
type Identity struct {
	WeComUserID  string
	FeishuUserID string
	PersonName   string // NAS 第一层目录名 / FastGPT 知识库名都以此为准
	UpdatedAt    int64
}

// FileIndexEntry 文件推送台账：NAS 上一份文件 ↔ FastGPT collection 的映射（幂等推送依据）。
type FileIndexEntry struct {
	Person       string
	Source       string // wecom / feishu
	NasPath      string // NAS 绝对路径
	Category     string // 分类（合同/方案/手册…）
	CollectionID string // FastGPT collection id（空=未推送）
	Frozen       bool
	Checksum     string // 内容校验（变了才重传）
	UpdatedAt    int64
}

// Open 打开（不存在则创建）数据库并执行建表迁移。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite: %w", err)
	}
	// SQLite 写不并发，1 个连接足够且能避免 database is locked
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS oauth_grants (
  platform          TEXT NOT NULL,
  external_user_id  TEXT NOT NULL,
  code              TEXT NOT NULL DEFAULT '',
  access_token      TEXT NOT NULL DEFAULT '',
  refresh_token     TEXT NOT NULL DEFAULT '',
  expire_at         INTEGER NOT NULL DEFAULT 0,
  refresh_expire_at INTEGER NOT NULL DEFAULT 0,
  status            TEXT NOT NULL DEFAULT 'authorized',
  updated_at        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (platform, external_user_id)
);
CREATE TABLE IF NOT EXISTS identities (
  wecom_user_id  TEXT PRIMARY KEY,
  feishu_user_id TEXT NOT NULL DEFAULT '',
  person_name    TEXT NOT NULL DEFAULT '',
  updated_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_identities_feishu ON identities(feishu_user_id);
CREATE TABLE IF NOT EXISTS file_index (
  person        TEXT NOT NULL,
  source        TEXT NOT NULL,
  nas_path      TEXT NOT NULL,
  category      TEXT NOT NULL DEFAULT '',
  collection_id TEXT NOT NULL DEFAULT '',
  frozen        INTEGER NOT NULL DEFAULT 0,
  checksum      TEXT NOT NULL DEFAULT '',
  updated_at    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (person, source, nas_path)
);
CREATE TABLE IF NOT EXISTS checkpoint (
  connector  TEXT PRIMARY KEY,
  cursor     TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS message_log (
  msgid      TEXT NOT NULL,
  person     TEXT NOT NULL,
  msg_time   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (msgid, person)
);
CREATE TABLE IF NOT EXISTS room_members (
  room_id    TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (room_id, user_id)
);
CREATE TABLE IF NOT EXISTS room_names (
  room_id    TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS feishu_doc_state (
  token      TEXT PRIMARY KEY,
  edit_time  INTEGER NOT NULL DEFAULT 0,
  title      TEXT NOT NULL DEFAULT '',
  owner_id   TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS feishu_doc_copy (
  token      TEXT NOT NULL,
  person_id  TEXT NOT NULL,
  via        TEXT NOT NULL DEFAULT '',
  edit_time  INTEGER NOT NULL DEFAULT 0,
  file_name  TEXT NOT NULL DEFAULT '',
  owner_id   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (token, person_id)
);
CREATE INDEX IF NOT EXISTS idx_feishu_doc_copy_person ON feishu_doc_copy(person_id);
CREATE TABLE IF NOT EXISTS feishu_doc_members (
  token      TEXT PRIMARY KEY,
  checked_at INTEGER NOT NULL DEFAULT 0,
  members    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS feishu_shared_root (
  viewer_id  TEXT NOT NULL,
  token      TEXT NOT NULL,
  doc_type   TEXT NOT NULL DEFAULT 'folder',
  PRIMARY KEY (viewer_id, token)
);
CREATE TABLE IF NOT EXISTS feishu_mail_folder (
  person_id  TEXT NOT NULL,
  folder_id  TEXT NOT NULL,
  scanned_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (person_id, folder_id)
);
CREATE TABLE IF NOT EXISTS feishu_mail_seen (
  person_id  TEXT NOT NULL,
  message_id TEXT NOT NULL,
  smtp_id    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (person_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_feishu_mail_seen_smtp ON feishu_mail_seen(person_id, smtp_id);
CREATE TABLE IF NOT EXISTS api_tokens (
  token_hash   TEXT PRIMARY KEY,
  display      TEXT NOT NULL DEFAULT '',
  person_id    TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'active',   -- active | revoked
  created_at   INTEGER NOT NULL DEFAULT 0,
  expires_at   INTEGER NOT NULL DEFAULT 0,       -- 0 = 不过期；轮换时给旧 token 设宽限期
  last_used_at INTEGER NOT NULL DEFAULT 0,
  revoked_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_tokens_person ON api_tokens(person_id);`)
	if err != nil {
		return fmt.Errorf("建表迁移: %w", err)
	}
	// 兼容已有表升级：若旧版本已创建 feishu_doc_copy 但缺少 kind 列，补充该字段
	if !s.hasColumn("feishu_doc_copy", "kind") {
		if _, err := s.db.Exec(`ALTER TABLE feishu_doc_copy ADD COLUMN kind TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("补充 feishu_doc_copy.kind 列: %w", err)
		}
	}
	// 兼容已有表升级：旧版共享表只记文件夹，没有 doc_type 列，补上并把已有记录视为文件夹
	if !s.hasColumn("feishu_shared_root", "doc_type") {
		if _, err := s.db.Exec(`ALTER TABLE feishu_shared_root ADD COLUMN doc_type TEXT NOT NULL DEFAULT 'folder'`); err != nil {
			return fmt.Errorf("补充 feishu_shared_root.doc_type 列: %w", err)
		}
	}
	// 从旧版升级：副本表为空时，用 feishu_doc_state 给所有者回填一份副本记录（只做一次），
	// 这样升级前已同步、升级后在飞书被删的文档也能被删除判断覆盖到。
	if _, err := s.db.Exec(`
INSERT OR IGNORE INTO feishu_doc_copy (token, person_id, via, edit_time, file_name, owner_id, kind, updated_at)
SELECT token, owner_id, 'owner', edit_time, title, owner_id,
       CASE WHEN token LIKE 'mail\_%' ESCAPE '\' THEN 'mail' ELSE '' END, updated_at
FROM feishu_doc_state
WHERE owner_id != '' AND title != '' AND NOT EXISTS (SELECT 1 FROM feishu_doc_copy)`); err != nil {
		return fmt.Errorf("回填飞书副本状态: %w", err)
	}
	return nil
}

func (s *Store) hasColumn(table, col string) bool {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil && name == col {
			return true
		}
	}
	return false
}

var ErrNotFound = errors.New("记录不存在")

// ===== OAuth 授权 =====

func (s *Store) UpsertOAuthGrant(g *OAuthGrant) error {
	g.UpdatedAt = time.Now().Unix()
	_, err := s.db.Exec(`
INSERT INTO oauth_grants (platform, external_user_id, code, access_token, refresh_token, expire_at, refresh_expire_at, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(platform, external_user_id) DO UPDATE SET
  code=excluded.code, access_token=excluded.access_token, refresh_token=excluded.refresh_token,
  expire_at=excluded.expire_at, refresh_expire_at=excluded.refresh_expire_at,
  status=excluded.status, updated_at=excluded.updated_at`,
		g.Platform, g.ExternalUserID, g.Code, g.AccessToken, g.RefreshToken,
		g.ExpireAt, g.RefreshExpireAt, g.Status, g.UpdatedAt)
	return err
}

func (s *Store) GetOAuthGrant(platform, externalUserID string) (*OAuthGrant, error) {
	row := s.db.QueryRow(`
SELECT platform, external_user_id, code, access_token, refresh_token, expire_at, refresh_expire_at, status, updated_at
FROM oauth_grants WHERE platform=? AND external_user_id=?`, platform, externalUserID)
	g := &OAuthGrant{}
	if err := row.Scan(&g.Platform, &g.ExternalUserID, &g.Code, &g.AccessToken, &g.RefreshToken,
		&g.ExpireAt, &g.RefreshExpireAt, &g.Status, &g.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return g, nil
}

// ListOAuthGrants 列出某平台全部授权记录（token refresher 与 /oauth/status 用）。
func (s *Store) ListOAuthGrants(platform string) ([]OAuthGrant, error) {
	rows, err := s.db.Query(`
SELECT platform, external_user_id, code, access_token, refresh_token, expire_at, refresh_expire_at, status, updated_at
FROM oauth_grants WHERE platform=?`, platform)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthGrant
	for rows.Next() {
		var g OAuthGrant
		if err := rows.Scan(&g.Platform, &g.ExternalUserID, &g.Code, &g.AccessToken, &g.RefreshToken,
			&g.ExpireAt, &g.RefreshExpireAt, &g.Status, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ===== 身份映射 =====

func (s *Store) UpsertIdentity(id *Identity) error {
	id.UpdatedAt = time.Now().Unix()
	_, err := s.db.Exec(`
INSERT INTO identities (wecom_user_id, feishu_user_id, person_name, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(wecom_user_id) DO UPDATE SET
  feishu_user_id=excluded.feishu_user_id, person_name=excluded.person_name, updated_at=excluded.updated_at`,
		id.WeComUserID, id.FeishuUserID, id.PersonName, id.UpdatedAt)
	return err
}

// ListIdentities 全部身份映射，按企微 userid 排序。
func (s *Store) ListIdentities() ([]Identity, error) {
	rows, err := s.db.Query(`
SELECT wecom_user_id, feishu_user_id, person_name, updated_at FROM identities ORDER BY wecom_user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.WeComUserID, &id.FeishuUserID, &id.PersonName, &id.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) GetIdentityByWeCom(wecomUserID string) (*Identity, error) {
	row := s.db.QueryRow(`
SELECT wecom_user_id, feishu_user_id, person_name, updated_at FROM identities WHERE wecom_user_id=?`, wecomUserID)
	return scanIdentity(row)
}

func (s *Store) GetIdentityByFeishu(feishuUserID string) (*Identity, error) {
	row := s.db.QueryRow(`
SELECT wecom_user_id, feishu_user_id, person_name, updated_at FROM identities WHERE feishu_user_id=?`, feishuUserID)
	return scanIdentity(row)
}

func scanIdentity(row *sql.Row) (*Identity, error) {
	id := &Identity{}
	if err := row.Scan(&id.WeComUserID, &id.FeishuUserID, &id.PersonName, &id.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return id, nil
}

// ===== 文件推送台账 =====

func (s *Store) UpsertFileIndex(e *FileIndexEntry) error {
	e.UpdatedAt = time.Now().Unix()
	frozen := 0
	if e.Frozen {
		frozen = 1
	}
	_, err := s.db.Exec(`
INSERT INTO file_index (person, source, nas_path, category, collection_id, frozen, checksum, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(person, source, nas_path) DO UPDATE SET
  category=excluded.category, collection_id=excluded.collection_id,
  frozen=excluded.frozen, checksum=excluded.checksum, updated_at=excluded.updated_at`,
		e.Person, e.Source, e.NasPath, e.Category, e.CollectionID, frozen, e.Checksum, e.UpdatedAt)
	return err
}

func (s *Store) GetFileIndex(person, source, nasPath string) (*FileIndexEntry, error) {
	row := s.db.QueryRow(`
SELECT person, source, nas_path, category, collection_id, frozen, checksum, updated_at
FROM file_index WHERE person=? AND source=? AND nas_path=?`, person, source, nasPath)
	e := &FileIndexEntry{}
	var frozen int
	if err := row.Scan(&e.Person, &e.Source, &e.NasPath, &e.Category, &e.CollectionID,
		&frozen, &e.Checksum, &e.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.Frozen = frozen == 1
	return e, nil
}

// ListFileIndexByPersonAndSource 列出某人某来源的所有已推送台账记录（用于 NAS 删除后的反向对账）。
func (s *Store) ListFileIndexByPersonAndSource(person, source string) ([]FileIndexEntry, error) {
	rows, err := s.db.Query(`
SELECT person, source, nas_path, category, collection_id, frozen, checksum, updated_at
FROM file_index WHERE person=? AND source=?`, person, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileIndexEntry
	for rows.Next() {
		var e FileIndexEntry
		var frozen int
		if err := rows.Scan(&e.Person, &e.Source, &e.NasPath, &e.Category, &e.CollectionID,
			&frozen, &e.Checksum, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Frozen = frozen == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteFileIndex 删除某条文件索引台账。
func (s *Store) DeleteFileIndex(person, source, nasPath string) error {
	_, err := s.db.Exec(`DELETE FROM file_index WHERE person=? AND source=? AND nas_path=?`, person, source, nasPath)
	return err
}

// ===== 连接器游标 =====

// GetCheckpoint 读取连接器游标；不存在返回空字符串（不报错，等价从头开始）。
func (s *Store) GetCheckpoint(connector string) (string, error) {
	var cursor string
	err := s.db.QueryRow(`SELECT cursor FROM checkpoint WHERE connector=?`, connector).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (s *Store) SetCheckpoint(connector, cursor string) error {
	_, err := s.db.Exec(`
INSERT INTO checkpoint (connector, cursor, updated_at) VALUES (?, ?, ?)
ON CONFLICT(connector) DO UPDATE SET cursor=excluded.cursor, updated_at=excluded.updated_at`,
		connector, cursor, time.Now().Unix())
	return err
}

// ===== 消息去重（markdown 滚动追加防重；粒度=消息×人，部分失败重试时只对没写成功的人补写）=====

// IsMessageLogged 某条消息是否已给某人写过。
func (s *Store) IsMessageLogged(msgid, person string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM message_log WHERE msgid=? AND person=?`, msgid, person).Scan(&n)
	return n > 0, err
}

// LogMessage 记录消息已给某人写过。
func (s *Store) LogMessage(msgid, person string, msgTime int64) error {
	_, err := s.db.Exec(`
INSERT INTO message_log (msgid, person, msg_time, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT(msgid, person) DO NOTHING`,
		msgid, person, msgTime, time.Now().Unix())
	return err
}

// ===== 群成员学习（企微群聊消息 tolist 为空，群成员无 API 可查，靠发言记录逐步积累）=====

// AddRoomParticipant 记录某人在某群发过言（学到一个成员）。
func (s *Store) AddRoomParticipant(roomID, userID string) error {
	if roomID == "" || userID == "" {
		return nil
	}
	_, err := s.db.Exec(`
INSERT INTO room_members (room_id, user_id, updated_at) VALUES (?, ?, ?)
ON CONFLICT(room_id, user_id) DO UPDATE SET updated_at=excluded.updated_at`,
		roomID, userID, time.Now().Unix())
	return err
}

// ===== 飞书文档变更状态（token → edit_time，增量拉取依据）=====

// ListRoomParticipants 返回已学习到的群成员 user_id 列表。
func (s *Store) ListRoomParticipants(roomID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT user_id FROM room_members WHERE room_id=?`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetRoomName 获取群聊名称，未配置返回空。
func (s *Store) GetRoomName(roomID string) string {
	var name string
	_ = s.db.QueryRow(`SELECT name FROM room_names WHERE room_id=?`, roomID).Scan(&name)
	return name
}

// SetRoomName 记录群聊名称。
func (s *Store) SetRoomName(roomID, name string) error {
	_, err := s.db.Exec(`
INSERT INTO room_names (room_id, name, updated_at) VALUES (?, ?, ?)
ON CONFLICT(room_id) DO UPDATE SET name=excluded.name, updated_at=excluded.updated_at`,
		roomID, name, time.Now().Unix())
	return err
}

// DocState 飞书文档状态记录。
type DocState struct {
	Token    string
	EditTime int64
	Title    string
	OwnerID  string
}

// ListDocStates 列出所有历史已同步的飞书文档记录。
func (s *Store) ListDocStates() ([]DocState, error) {
	rows, err := s.db.Query(`SELECT token, edit_time, title, owner_id FROM feishu_doc_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DocState
	for rows.Next() {
		var d DocState
		if err := rows.Scan(&d.Token, &d.EditTime, &d.Title, &d.OwnerID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDocStatesByUser 列出属于某个特定用户的历史已同步飞书文档记录（用于单人作用域差集比对）。
func (s *Store) ListDocStatesByUser(ownerID string) ([]DocState, error) {
	rows, err := s.db.Query(`SELECT token, edit_time, title, owner_id FROM feishu_doc_state WHERE owner_id=?`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DocState
	for rows.Next() {
		var d DocState
		if err := rows.Scan(&d.Token, &d.EditTime, &d.Title, &d.OwnerID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDocState 删除某篇文档的同步状态记录。
func (s *Store) DeleteDocState(token string) error {
	_, err := s.db.Exec(`DELETE FROM feishu_doc_state WHERE token=?`, token)
	return err
}

// GetDocEditTime 返回已记录的文档修改时间（秒）；未见过返回 0。
func (s *Store) GetDocEditTime(token string) int64 {
	var v int64
	if err := s.db.QueryRow(`SELECT edit_time FROM feishu_doc_state WHERE token=?`, token).Scan(&v); err != nil {
		return 0
	}
	return v
}

// UpsertDocState 记录文档本次已同步到的修改时间。
func (s *Store) UpsertDocState(token, title, ownerID string, editTime int64) error {
	_, err := s.db.Exec(`
INSERT INTO feishu_doc_state (token, edit_time, title, owner_id, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(token) DO UPDATE SET edit_time=excluded.edit_time, title=excluded.title,
  owner_id=excluded.owner_id, updated_at=excluded.updated_at`,
		token, editTime, title, ownerID, time.Now().Unix())
	return err
}

// ===== 飞书文档副本（一篇文档 × 一个人 = NAS 上的一份文件）=====

// DocCopy 某人目录下的一份飞书文档副本。
// Via：owner=所有者本人；scan=该授权员工扫描时能看到；collab=来自协作者列表。
type DocCopy struct {
	Token    string
	PersonID string
	Via      string
	EditTime int64
	FileName string
	OwnerID  string
	Kind     string // 记录类型（docx/sheet/.../mail/minutes），删除判断按类型区分
}

func (s *Store) queryCopies(where string, arg string) ([]DocCopy, error) {
	rows, err := s.db.Query(`SELECT token, person_id, via, edit_time, file_name, owner_id, kind
FROM feishu_doc_copy WHERE `+where, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocCopy
	for rows.Next() {
		var c DocCopy
		if err := rows.Scan(&c.Token, &c.PersonID, &c.Via, &c.EditTime, &c.FileName, &c.OwnerID, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCopies 列出一篇文档的全部副本。
func (s *Store) ListCopies(token string) ([]DocCopy, error) {
	return s.queryCopies("token=?", token)
}

// ListCopiesByPerson 列出某人目录下的全部飞书文档副本。
func (s *Store) ListCopiesByPerson(personID string) ([]DocCopy, error) {
	return s.queryCopies("person_id=?", personID)
}

// UpsertCopy 记录某人的副本已写入 NAS。
func (s *Store) UpsertCopy(c DocCopy) error {
	_, err := s.db.Exec(`
INSERT INTO feishu_doc_copy (token, person_id, via, edit_time, file_name, owner_id, kind, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(token, person_id) DO UPDATE SET via=excluded.via, edit_time=excluded.edit_time,
  file_name=excluded.file_name, owner_id=excluded.owner_id, kind=excluded.kind, updated_at=excluded.updated_at`,
		c.Token, c.PersonID, c.Via, c.EditTime, c.FileName, c.OwnerID, c.Kind, time.Now().Unix())
	return err
}

// DeleteCopy 删除某人的一份副本记录。
func (s *Store) DeleteCopy(token, personID string) error {
	_, err := s.db.Exec(`DELETE FROM feishu_doc_copy WHERE token=? AND person_id=?`, token, personID)
	return err
}

// GetDocMembers 返回某篇文档上次查到的协作者（open_id 列表）及查询时间（秒）；没查过返回 0。
func (s *Store) GetDocMembers(token string) (checkedAt int64, members []string) {
	var list string
	if err := s.db.QueryRow(`SELECT checked_at, members FROM feishu_doc_members WHERE token=?`, token).
		Scan(&checkedAt, &list); err != nil {
		return 0, nil
	}
	if list != "" {
		members = strings.Split(list, ",")
	}
	return checkedAt, members
}

// SetDocMembers 记录某篇文档本次查到的协作者及查询时间。
func (s *Store) SetDocMembers(token string, ts int64, members []string) error {
	_, err := s.db.Exec(`
INSERT INTO feishu_doc_members (token, checked_at, members) VALUES (?, ?, ?)
ON CONFLICT(token) DO UPDATE SET checked_at=excluded.checked_at, members=excluded.members`,
		token, ts, strings.Join(members, ","))
	return err
}

// ===== 别人共享给员工的文件夹和智能纪要（搜索发现，每轮用员工身份查）=====

// SharedRoot 员工能看到、但不在他云盘树下的一个共享文件夹或一篇智能纪要。
type SharedRoot struct {
	ViewerID string
	Token    string
	DocType  string // folder / docx
}

// ListSharedRoots 列出某员工已发现的共享文件夹和智能纪要。
func (s *Store) ListSharedRoots(viewerID string) ([]SharedRoot, error) {
	rows, err := s.db.Query(`SELECT viewer_id, token, doc_type FROM feishu_shared_root WHERE viewer_id=?`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedRoot
	for rows.Next() {
		var r SharedRoot
		if err := rows.Scan(&r.ViewerID, &r.Token, &r.DocType); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertSharedRoot 记录一次发现（已记录过的不重复写）。
func (s *Store) UpsertSharedRoot(r SharedRoot) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO feishu_shared_root (viewer_id, token, doc_type) VALUES (?, ?, ?)`, r.ViewerID, r.Token, r.DocType)
	return err
}

// DeleteSharedRoot 员工已看不到（无权限或已删除）时移除。
func (s *Store) DeleteSharedRoot(viewerID, token string) error {
	_, err := s.db.Exec(`DELETE FROM feishu_shared_root WHERE viewer_id=? AND token=?`, viewerID, token)
	return err
}

// ===== 飞书邮箱：每个文件夹上次完整翻完、且没有漏存邮件的时间 =====

// MailFolderScannedAt 某员工某个邮箱文件夹上次完整核对通过的时间（秒），没有记录返回 0。
func (s *Store) MailFolderScannedAt(personID, folderID string) int64 {
	var ts int64
	_ = s.db.QueryRow(`SELECT scanned_at FROM feishu_mail_folder WHERE person_id=? AND folder_id=?`, personID, folderID).Scan(&ts)
	return ts
}

// SetMailFolderScannedAt 记录一次完整核对通过。
func (s *Store) SetMailFolderScannedAt(personID, folderID string, ts int64) error {
	_, err := s.db.Exec(`
INSERT INTO feishu_mail_folder (person_id, folder_id, scanned_at) VALUES (?, ?, ?)
ON CONFLICT(person_id, folder_id) DO UPDATE SET scanned_at=excluded.scanned_at`, personID, folderID, ts)
	return err
}

// MailSeen 这封邮件（按 message_id）是否已给该员工处理过：存过，或是已存邮件的另一份拷贝。
func (s *Store) MailSeen(personID, messageID string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM feishu_mail_seen WHERE person_id=? AND message_id=?`, personID, messageID).Scan(&n)
	return n > 0
}

// MailSeenSMTP 同一封邮件（按 RFC 邮件 ID）是否已给该员工存过。
func (s *Store) MailSeenSMTP(personID, smtpID string) bool {
	if smtpID == "" {
		return false
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM feishu_mail_seen WHERE person_id=? AND smtp_id=?`, personID, smtpID).Scan(&n)
	return n > 0
}

// AddMailSeen 记录一封已处理的邮件。
func (s *Store) AddMailSeen(personID, messageID, smtpID string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO feishu_mail_seen (person_id, message_id, smtp_id) VALUES (?, ?, ?)`, personID, messageID, smtpID)
	return err
}

// ===== API token =====

// APIToken 一枚员工专属 API token 的存库信息（不含明文与哈希）。
// PersonID 现阶段是 identities.wecom_user_id；Status 为 active 或 revoked；
// 时间均为 unix 秒，ExpiresAt=0 表示不过期。
// 明文只在签发/轮换时返回一次，库里只存 SHA-256 哈希。
type APIToken struct {
	Display    string
	PersonID   string
	Status     string
	CreatedAt  int64
	ExpiresAt  int64
	LastUsedAt int64
	RevokedAt  int64
}

// tokenStatusActive token 有效状态值。
const tokenStatusActive = "active"

// tokenStatusRevoked token 已吊销状态值。
const tokenStatusRevoked = "revoked"

// tokenLastUsedThrottle 同一 token 的 last_used_at 最多每 60 秒写一次，避免每个请求都写库。
const tokenLastUsedThrottle = 60

// execer 让签发逻辑既能跑在 *sql.DB 上，也能跑在事务里。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// issueAPIToken 生成明文并写入一行 api_tokens。
func issueAPIToken(ex execer, personID string, now int64) (string, error) {
	token, err := apitoken.Generate()
	if err != nil {
		return "", err
	}
	if _, err := ex.Exec(`
INSERT INTO api_tokens (token_hash, display, person_id, status, created_at, expires_at, last_used_at, revoked_at)
VALUES (?, ?, ?, ?, ?, 0, 0, 0)`,
		apitoken.Hash(token), apitoken.Display(token), personID, tokenStatusActive, now); err != nil {
		return "", fmt.Errorf("写入 api_tokens: %w", err)
	}
	return token, nil
}

// IssueAPIToken 为员工签发新 token，返回明文（只此一次）。
func (s *Store) IssueAPIToken(personID string) (string, error) {
	if strings.TrimSpace(personID) == "" {
		return "", errors.New("签发 API token 需要 personID")
	}
	return issueAPIToken(s.db, personID, time.Now().Unix())
}

// RotateAPIToken 签发新 token；该员工现有 active 且未设过期的 token 改为 now+grace 过期。返回新明文。
// 新 token 立即生效，旧 token 在宽限期内仍可用，grace<=0 时旧 token 立刻过期。
func (s *Store) RotateAPIToken(personID string, grace time.Duration) (string, error) {
	if strings.TrimSpace(personID) == "" {
		return "", errors.New("轮换 API token 需要 personID")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	// 先给旧 token 设宽限期，再签发新 token，否则新 token 也会被设上过期时间。
	now := time.Now().Unix()
	expiresAt := now + int64(grace/time.Second)
	if _, err := tx.Exec(`
UPDATE api_tokens SET expires_at=? WHERE person_id=? AND status=? AND expires_at=0`,
		expiresAt, personID, tokenStatusActive); err != nil {
		return "", fmt.Errorf("设置旧 token 宽限期: %w", err)
	}
	token, err := issueAPIToken(tx, personID, now)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeAPITokens 吊销该员工全部 token（status=revoked, revoked_at=now），返回吊销条数。
func (s *Store) RevokeAPITokens(personID string) (int, error) {
	if strings.TrimSpace(personID) == "" {
		return 0, errors.New("吊销 API token 需要 personID")
	}
	res, err := s.db.Exec(`
UPDATE api_tokens SET status=?, revoked_at=? WHERE person_id=? AND status!=?`,
		tokenStatusRevoked, time.Now().Unix(), personID, tokenStatusRevoked)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// LookupAPIToken 明文 → 员工 ID。不存在、已吊销、已过期（expires_at>0 且 <=now）都返回 ("", false, nil)。
// 命中时更新 last_used_at（同一 token 60 秒内不重复写，避免每个请求都写库）。
func (s *Store) LookupAPIToken(token string) (personID string, ok bool, err error) {
	if token == "" {
		return "", false, nil
	}
	var (
		status     string
		expiresAt  int64
		lastUsedAt int64
	)
	err = s.db.QueryRow(`
SELECT person_id, status, expires_at, last_used_at FROM api_tokens WHERE token_hash=?`,
		apitoken.Hash(token)).Scan(&personID, &status, &expiresAt, &lastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	now := time.Now().Unix()
	if status != tokenStatusActive || (expiresAt > 0 && expiresAt <= now) {
		return "", false, nil
	}
	if now-lastUsedAt >= tokenLastUsedThrottle {
		if _, err := s.db.Exec(`UPDATE api_tokens SET last_used_at=? WHERE token_hash=?`,
			now, apitoken.Hash(token)); err != nil {
			return "", false, err
		}
	}
	return personID, true, nil
}

// ListAPITokens 列出全部 token（不含明文和哈希），按 person_id、created_at 排序。
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`
SELECT display, person_id, status, created_at, expires_at, last_used_at, revoked_at
FROM api_tokens ORDER BY person_id, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.Display, &t.PersonID, &t.Status, &t.CreatedAt,
			&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasActiveAPIToken 员工是否已有有效 token（用于自动签发时跳过）。
func (s *Store) HasActiveAPIToken(personID string) (bool, error) {
	if strings.TrimSpace(personID) == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM api_tokens WHERE person_id=? AND status=? AND (expires_at=0 OR expires_at>?)`,
		personID, tokenStatusActive, time.Now().Unix()).Scan(&n)
	return n > 0, err
}

// ListFeishuUserIDs 人名映射表里全部员工的飞书 open_id（去重、去空）。
func (s *Store) ListFeishuUserIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT feishu_user_id FROM identities WHERE feishu_user_id != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
