// 飞书 Wiki API：知识库空间与节点遍历。
//
// 为什么需要它：飞书新版把「我的文档库」归入 wiki 体系。用 drive/explorer/v2
// 列根目录会返回 code=0 但 children 为空——接口成功、结果为零，非常容易误判成
// 「账号里没有文档」。实测（2026-10-22）：用户云空间确实有文档，drive 接口全空，
// 必须走 wiki/v2/spaces 才能列出来。
//
// 所需权限：wiki:wiki:readonly（缺则报 99991679）。
package feishu

import (
	"net/url"
)

// WikiSpace 知识库空间。
type WikiSpace struct {
	SpaceID   string `json:"space_id"`
	Name      string `json:"name"`
	SpaceType string `json:"space_type"` // team / person
	Visibility string `json:"visibility"`
}

// WikiNode 知识库节点（文档或子目录）。
type WikiNode struct {
	Token      string `json:"node_token"`  // 节点 token（遍历子节点用）
	ObjToken   string `json:"obj_token"`   // 实际文档 token（下载内容用）
	ObjType    string `json:"obj_type"`    // doc/docx/sheet/bitable/file/slides/mindnote
	Title      string `json:"title"`
	ParentNode string `json:"parent_node_token"`
	HasChild   bool   `json:"has_child"`
	NodeType   string `json:"node_type"` // origin / shortcut

	// NodeCreateTime / NodeEditTime 是节点自身的创建/编辑时间（秒级字符串）。
	// 没有文档级的「内容修改时间」，用节点编辑时间做变更判断已经够用：
	// 文档正文改动会刷新节点 edit_time。
	NodeCreateTime string `json:"node_create_time"`
	NodeEditTime   string `json:"node_edit_time"`
}

// ListWikiSpaces 列出调用者可访问的知识库空间（自动翻页）。
func (c *Client) ListWikiSpaces(token string) ([]WikiSpace, error) {
	var all []WikiSpace
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "50")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var out struct {
			apiResp
			Data struct {
				Items     []WikiSpace `json:"items"`
				HasMore   bool        `json:"has_more"`
				PageToken string      `json:"page_token"`
			} `json:"data"`
		}
		if err := c.get("/wiki/v2/spaces", token, q, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data.Items...)
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}
	return all, nil
}

// ListWikiNodes 列出某个空间（或某个父节点）下的子节点（自动翻页）。
// parentNodeToken 为空表示列出空间的根节点。
func (c *Client) ListWikiNodes(token, spaceID, parentNodeToken string) ([]WikiNode, error) {
	var all []WikiNode
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "50")
		if parentNodeToken != "" {
			q.Set("parent_node_token", parentNodeToken)
		}
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var out struct {
			apiResp
			Data struct {
				Items     []WikiNode `json:"items"`
				HasMore   bool       `json:"has_more"`
				PageToken string     `json:"page_token"`
			} `json:"data"`
		}
		if err := c.get("/wiki/v2/spaces/"+spaceID+"/nodes", token, q, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data.Items...)
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}
	return all, nil
}

// WalkWikiSpace 深度遍历一个知识库空间的所有节点。
// 每个节点的 ObjToken/ObjType 已经是可下载的文档标识，无需二次解析。
func (c *Client) WalkWikiSpace(token, spaceID string, maxDepth int, fn func(WikiSpace, WikiNode)) error {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	var walk func(parent string, depth int) error
	walk = func(parent string, depth int) error {
		if depth > maxDepth {
			return nil
		}
		nodes, err := c.ListWikiNodes(token, spaceID, parent)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			fn(WikiSpace{SpaceID: spaceID}, n)
			if n.HasChild {
				if err := walk(n.Token, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("", 0)
}
