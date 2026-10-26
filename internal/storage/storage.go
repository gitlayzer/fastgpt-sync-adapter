// Package storage SQLite 存储层：授权、身份映射、文件推送台账、连接器游标。
// 用纯 Go 的 modernc.org/sqlite（不依赖 cgo，避免与企微 SDK 的 cgo 构建冲突，交叉编译也省事）。
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
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
);`)
	if err != nil {
		return fmt.Errorf("建表迁移: %w", err)
	}
	return nil
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
