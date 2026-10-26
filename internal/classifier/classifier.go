// Package classifier 文件用途分类器（R4：客户确认无 LLM 端点，分类由中间件用可配规则承担）。
// 分类结果只用于 NAS 归档子目录与 .meta.json 侧边车，绝不改动原文件内容（不清洗红线）。
package classifier

import (
	"strings"
)

// Rule 一条分类规则：命中任一关键词即归为 label。
type Rule struct {
	Label    string   `yaml:"label"`    // 分类名，如 合同/方案/手册
	Keywords []string `yaml:"keywords"` // 关键词（匹配文件名与正文摘要，不区分大小写）
}

// Config 分类器配置。
type Config struct {
	Enabled      bool   `yaml:"enabled"`
	DefaultLabel string `yaml:"default_label"` // 未命中任何规则时的归类，默认 "其他"
	Rules        []Rule `yaml:"rules"`
}

// ApplyDefaults 填充默认值（规则为空时给一套常用起点，用户可在配置里覆盖）。
func (c *Config) ApplyDefaults() {
	if c.DefaultLabel == "" {
		c.DefaultLabel = "其他"
	}
	if len(c.Rules) == 0 {
		c.Rules = []Rule{
			{Label: "合同", Keywords: []string{"合同", "协议", "contract", "agreement", "订单", "采购"}},
			{Label: "方案", Keywords: []string{"方案", "plan", "proposal", "设计", "规划"}},
			{Label: "手册", Keywords: []string{"手册", "说明书", "指南", "manual", "guide", "handbook", "白皮书"}},
			{Label: "专利", Keywords: []string{"专利", "patent", "cn1"}},
			{Label: "报告", Keywords: []string{"报告", "report", "周报", "月报", "总结"}},
			{Label: "数据表", Keywords: []string{"数据", "报表", "台账", "清单", ".xlsx", ".csv"}},
		}
	}
}

// Classifier 规则分类器。
type Classifier struct {
	cfg Config
}

func New(cfg Config) *Classifier {
	c := cfg
	c.ApplyDefaults()
	return &Classifier{cfg: c}
}

// Classify 按文件名（+可选正文摘要）匹配关键词，返回分类标签。
// 匹配不区分大小写；多条命中时取先定义的规则。
func (c *Classifier) Classify(name, textSample string) string {
	haystack := strings.ToLower(name)
	if textSample != "" {
		haystack += "\n" + strings.ToLower(textSample)
	}
	for _, r := range c.cfg.Rules {
		for _, kw := range r.Keywords {
			if kw != "" && strings.Contains(haystack, strings.ToLower(kw)) {
				return r.Label
			}
		}
	}
	return c.cfg.DefaultLabel
}

// Enabled 分类是否启用。
func (c *Classifier) Enabled() bool { return c.cfg.Enabled }
