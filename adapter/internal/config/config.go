package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 中间件三方配置。
type Config struct {
	// 1. FastGPT：连接、上传知识库；并用其用户列表在 NAS 建目录
	FastGPT FastGPTConfig `yaml:"fastgpt"`
	// 2. 群晖 NAS：企微/飞书文件落盘位置
	NAS NASConfig `yaml:"nas"`
	// 3. 企业微信会话内容存档
	WeCom WeComConfig `yaml:"wecom"`

	// 同步行为
	Sync SyncConfig `yaml:"sync"`
}

type FastGPTConfig struct {
	BaseURL   string `yaml:"base_url"`   // 如 https://aanshixbkhrk.sealoshzh.site
	APIKey    string `yaml:"api_key"`    // fastgpt-xxx（需账号级，能调用户列表接口）
	DatasetID string `yaml:"dataset_id"` // 目标知识库 id
	// 上传解析参数
	ImageIndex     bool `yaml:"image_index"`
	AutoIndexes    bool `yaml:"auto_indexes"`
	CustomPdfParse bool `yaml:"custom_pdf_parse"`
}

type NASConfig struct {
	URL      string `yaml:"url"`      // 群晖地址，如 http://192.168.1.10:5000
	Username string `yaml:"username"` // 群晖账号
	Password string `yaml:"password"` // 群晖密码
	// 落盘根路径：企微/飞书数据按「用户名」在此下建子目录
	// 例: /fastgpt-sync → /fastgpt-sync/zhangsan/...
	RootPath string `yaml:"root_path"`
}

type WeComConfig struct {
	CorpID string `yaml:"corp_id"` // 企业 corpid
	Secret string `yaml:"secret"`  // 会话存档 secret
	// RSA 私钥：可直接给 PEM 文本，或给文件路径
	RSAPrivateKey     string `yaml:"rsa_private_key"`      // PEM 文本（与 file 二选一）
	RSAPrivateKeyFile string `yaml:"rsa_private_key_file"` // PEM 文件路径
	// 可选：企微会话存档 SDK 动态库路径（libWeWorkFinanceSdk_C.so / .dylib）
	SDKLibPath string `yaml:"sdk_lib_path"`
}

type SyncConfig struct {
	Interval   time.Duration `yaml:"interval"`   // 上传/同步间隔
	StateFile  string        `yaml:"state_file"` // 增量状态文件
	RunOnStart bool          `yaml:"run_on_start"`
	// NAS 文件被删除后，是否同步删除 FastGPT 里的对应文件。
	// 默认 false（保留）：NAS 是单向归集终点，为溯源默认保留远端副本；
	// 设为 true 则本地删除时同步删除远端。
	SyncDelete bool `yaml:"sync_delete"`
}

// Load 从 YAML 配置文件加载。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate 校验必填项。
func (c *Config) Validate() error {
	if c.FastGPT.BaseURL == "" {
		return fmt.Errorf("fastgpt.base_url 必填")
	}
	if c.FastGPT.APIKey == "" {
		return fmt.Errorf("fastgpt.api_key 必填")
	}
	if c.FastGPT.DatasetID == "" {
		return fmt.Errorf("fastgpt.dataset_id 必填")
	}
	if c.NAS.URL == "" || c.NAS.Username == "" || c.NAS.Password == "" {
		return fmt.Errorf("nas.url / nas.username / nas.password 必填")
	}
	if c.NAS.RootPath == "" {
		return fmt.Errorf("nas.root_path 必填")
	}
	// 企微可选（飞书链路可单独跑），但若配了就要配全
	w := c.WeCom
	partial := w.CorpID != "" || w.Secret != "" || w.RSAPrivateKey != "" || w.RSAPrivateKeyFile != ""
	if partial {
		if w.CorpID == "" || w.Secret == "" {
			return fmt.Errorf("wecom.corp_id / wecom.secret 需同时配置")
		}
		if w.RSAPrivateKey == "" && w.RSAPrivateKeyFile == "" {
			return fmt.Errorf("wecom.rsa_private_key 或 wecom.rsa_private_key_file 需配置其一")
		}
	}
	// 默认值
	if c.Sync.Interval <= 0 {
		c.Sync.Interval = 10 * time.Minute
	}
	if c.Sync.StateFile == "" {
		c.Sync.StateFile = "./sync_state.json"
	}
	return nil
}

// WeComPrivateKeyPEM 返回 RSA 私钥 PEM 内容。
func (c *Config) WeComPrivateKeyPEM() (string, error) {
	if c.WeCom.RSAPrivateKey != "" {
		return c.WeCom.RSAPrivateKey, nil
	}
	data, err := os.ReadFile(c.WeCom.RSAPrivateKeyFile)
	if err != nil {
		return "", fmt.Errorf("读取 RSA 私钥文件: %w", err)
	}
	return string(data), nil
}
