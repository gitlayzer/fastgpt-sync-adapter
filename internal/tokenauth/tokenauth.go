// Package tokenauth 把员工专属 API token 解析成「员工 → NAS 根目录」，实现 server.Authenticator。
//
// 根目录 = <nasRoot>/<identities.person_name>（v1 目录结构）。token 绑定员工 ID 而不是路径：
// 员工改名后目录跟着变，服务端每次按员工 ID 重新查当前目录，不用重发 token。
// P2 落地 NAS 落库标准 v2 后，只需要改 RootOf 的拼法。
package tokenauth

import (
	"strings"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// Auth 员工 token 解析器。
type Auth struct {
	store   *storage.Store
	nasRoot string
}

// New 构造解析器。nasRoot 为整个 NAS 根路径（如 /FastGPT-test），每员工根目录挂在其下。
func New(store *storage.Store, nasRoot string) *Auth {
	return &Auth{
		store:   store,
		nasRoot: strings.TrimRight(strings.TrimSpace(nasRoot), "/"),
	}
}

// ResolveToken 明文 token → 员工 ID + 员工根目录。
// token 不存在、已吊销、已过期、或员工身份/姓名拿不到目录时都返回 ok=false。
func (a *Auth) ResolveToken(token string) (personID, root string, ok bool, err error) {
	if a == nil || a.store == nil || strings.TrimSpace(token) == "" {
		return "", "", false, nil
	}
	personID, ok, err = a.store.LookupAPIToken(token)
	if err != nil || !ok {
		return "", "", false, err
	}
	root, ok = a.RootOf(personID)
	if !ok {
		// token 本身有效但当前找不到员工目录（身份缺失 / 姓名非法）：
		// 按无效 token 处理（401），不返回 500，避免把内部状态暴露给调用方。
		return "", "", false, nil
	}
	return personID, root, true, nil
}

// RootOf 员工 ID → 当前根目录（签名下载链接校验时用，员工目录改名后仍能找到）。
func (a *Auth) RootOf(personID string) (string, bool) {
	if a == nil || a.store == nil || a.nasRoot == "" {
		return "", false
	}
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return "", false
	}
	id, err := a.store.GetIdentityByWeCom(personID)
	if err != nil || id == nil {
		return "", false
	}
	name := strings.TrimSpace(id.PersonName)
	if !ValidPersonName(name) {
		return "", false
	}
	return a.nasRoot + "/" + name, true
}

// ValidPersonName 判断 person_name 能否安全地当作目录名拼到 NAS 根下。
// 拒绝空、"."、".."、含路径分隔符（/ 与 \）以及含控制字符的名字，防止路径穿越。
func ValidPersonName(name string) bool {
	switch name {
	case "", ".", "..":
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
