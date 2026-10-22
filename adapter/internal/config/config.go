// Package config 配置加载。结构按「引擎 / 连接器 / Sink」三组组织：
// connectors 下每个数据源一节（enabled 开关 + 各自凭证），sinks 下每个出口一节。
// 出口二选一：nas（归档）与 fastgpt（附件直传知识库）同时只能启用一个。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"fastgpt-sync-adapter/adapter/internal/classifier"
	"fastgpt-sync-adapter/adapter/internal/connector/wecom"
	nassink "fastgpt-sync-adapter/adapter/internal/sink/nas"
)

// Duration 支持 YAML 里写 "60s" / "10m" 这类字符串，也兼容整数（纳秒）。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if v, err := time.ParseDuration(s); err == nil {
		*d = Duration(v)
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return fmt.Errorf("非法时长 %q（示例: 60s / 10m / 1h）", s)
	}
	*d = Duration(n)
	return nil
}

// D 返回 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config 顶层配置。
type Config struct {
	Engine EngineConfig `yaml:"engine"`

	Connectors struct {
		WeCom WeComConnectorConfig `yaml:"wecom"`
		Feishu FeishuConnectorConfig `yaml:"feishu"`
	} `yaml:"connectors"`

	// Classifier 文件用途分类器（可配规则；客户决策：无 LLM 端点，先用规则关键词）
	Classifier classifier.Config `yaml:"classifier"`

	// Storage SQLite 存储（授权/身份/文件台账/游标）
	Storage StorageConfig `yaml:"storage"`

	// Auth 飞书 OAuth 授权 webhook（常驻，与 daemon 同进程）
	Auth AuthConfig `yaml:"auth"`

	// Identity 身份解析（企微 userid → 飞书用户 → 姓名；多维表格未到位前用本地映射）
	Identity IdentityConfig `yaml:"identity"`

	Sinks struct {
		NAS     NASSinkConfig     `yaml:"nas"`
		FastGPT FastGPTSinkConfig `yaml:"fastgpt"`
	} `yaml:"sinks"`
}

// EngineConfig 引擎全局配置。
type EngineConfig struct {
	StateFile  string `yaml:"state_file"` // 各连接器 checkpoint 统一存放
	RunOnStart bool   `yaml:"run_on_start"`
}

// WeComConnectorConfig 企微连接器配置节。
type WeComConnectorConfig struct {
	Enabled    bool         `yaml:"enabled"`
	Interval   Duration     `yaml:"interval"`    // 拉取间隔（需远小于媒体附件 3 天保留期，建议分钟级）
	BatchLimit int          `yaml:"batch_limit"` // 单批条数，默认 100（SDK 上限 1000）
	WeCom      wecom.Config `yaml:",inline"`     // corp_id/secret/rsa_private_key(_file)/sdk_lib_path
}

// FeishuConnectorConfig 飞书连接器配置节。
type FeishuConnectorConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Interval   Duration `yaml:"interval"`
	BatchLimit int      `yaml:"batch_limit"`
	AppID      string   `yaml:"app_id"`
	AppSecret  string   `yaml:"app_secret"`
	// Mode: tenant（应用身份遍历共享区/知识库）/ user（按已授权用户逐个拉个人云文档）/ both
	Mode string `yaml:"mode"`
	// Folders tenant 模式遍历的共享文件夹 token 列表（拥有者把文件夹分享给应用后填 token）
	Folders []string `yaml:"folders"`
}

// StorageConfig SQLite 存储配置。
type StorageConfig struct {
	DBFile string `yaml:"db_file"` // 如 ./adapter.db
}

// AuthConfig 飞书 OAuth 授权 webhook 配置。
type AuthConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Addr        string `yaml:"addr"`         // 监听地址，如 :8089
	ExternalURL string `yaml:"external_url"` // 外部可访问地址（拼 redirect_uri 用，需与飞书后台一致）
	// RefreshInterval token 刷新巡检间隔（默认 1h）
	RefreshInterval Duration `yaml:"refresh_interval"`
}

// IdentityConfig 身份解析配置。
type IdentityConfig struct {
	// Source: local（本地 JSON 映射文件，兜底）/ bitable（飞书多维表格，TODO 待表格到位）
	Source    string `yaml:"source"`
	LocalFile string `yaml:"local_file"` // source=local 时的映射文件路径
	Bitable   struct {
		AppToken string `yaml:"app_token"` // TODO: 多维表格 app_token
		TableID  string `yaml:"table_id"`  // TODO: 表 id
	} `yaml:"bitable"`
}

// FastGPTSinkConfig FastGPT 推送 sink 配置（方案 A：按人建库主动推）。
type FastGPTSinkConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BaseURL  string `yaml:"base_url"` // https://cloud.fastgpt.cn
	APIKey   string `yaml:"api_key"`
	Interval Duration `yaml:"interval"` // 推送巡检间隔
}

// UserDirsConfig 按 FastGPT 用户列表在 NAS 自动建目录（账号级 api_key）。
type UserDirsConfig struct {
	Enabled        bool   `yaml:"enabled"`
	FastGPTBaseURL string `yaml:"fastgpt_base_url"`
	FastGPTAPIKey  string `yaml:"fastgpt_api_key"` // 必须账号级，知识库级 key 无用户列表权限
}

// NASSinkConfig NAS Sink 配置节（归档出口：消息体+附件原样落盘）。
type NASSinkConfig struct {
	Enabled  bool `yaml:"enabled"`
	UserDirs UserDirsConfig `yaml:"user_dirs"`
	nassink.Config `yaml:",inline"`
}

// Load 从 YAML 配置文件加载并校验。
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

// Validate 校验必填项（仅校验 enabled 的模块）并填默认值。
func (c *Config) Validate() error {
	if c.Engine.StateFile == "" {
		c.Engine.StateFile = "./connector_state.json"
	}
	if c.Storage.DBFile == "" {
		c.Storage.DBFile = "./adapter.db"
	}

	// 企微连接器
	if w := &c.Connectors.WeCom; w.Enabled {
		if w.WeCom.CorpID == "" || w.WeCom.Secret == "" {
			return fmt.Errorf("connectors.wecom.corp_id / secret 必填")
		}
		if w.WeCom.RSAPrivateKey == "" && w.WeCom.RSAPrivateKeyFile == "" {
			return fmt.Errorf("connectors.wecom.rsa_private_key 或 rsa_private_key_file 需配置其一")
		}
		if w.Interval.D() <= 0 {
			w.Interval = Duration(60 * time.Second)
		}
		if w.BatchLimit <= 0 {
			w.BatchLimit = 100
		}
	}

	// 飞书连接器
	if f := &c.Connectors.Feishu; f.Enabled {
		if f.AppID == "" || f.AppSecret == "" {
			return fmt.Errorf("connectors.feishu.app_id / app_secret 必填")
		}
		if f.Mode == "" {
			f.Mode = "tenant"
		}
		if f.Interval.D() <= 0 {
			f.Interval = Duration(5 * time.Minute)
		}
		if f.BatchLimit <= 0 {
			f.BatchLimit = 50
		}
	}

	// 授权 webhook：user 模式必须开授权（否则拿不到用户 token）
	if a := &c.Auth; a.Enabled {
		if a.Addr == "" {
			a.Addr = ":8089"
		}
		if a.RefreshInterval.D() <= 0 {
			a.RefreshInterval = Duration(time.Hour)
		}
		if !c.Connectors.Feishu.Enabled || c.Connectors.Feishu.AppID == "" {
			return fmt.Errorf("auth 启用时 connectors.feishu 必须启用并配置 app_id/app_secret")
		}
	}
	if c.Connectors.Feishu.Enabled && c.Connectors.Feishu.Mode != "tenant" && !c.Auth.Enabled {
		return fmt.Errorf("connectors.feishu.mode=%s 需要启用 auth（OAuth 授权 webhook）", c.Connectors.Feishu.Mode)
	}

	// 身份解析
	if c.Identity.Source == "" {
		c.Identity.Source = "local"
	}
	if c.Identity.Source == "local" && c.Identity.LocalFile == "" {
		c.Identity.LocalFile = "./identity_map.json"
	}
	if c.Identity.Source == "bitable" && (c.Identity.Bitable.AppToken == "" || c.Identity.Bitable.TableID == "") {
		return fmt.Errorf("identity.source=bitable 时 bitable.app_token / table_id 必填")
	}

	// NAS Sink
	if n := &c.Sinks.NAS; n.Enabled {
		if n.URL == "" || n.Username == "" || n.Password == "" {
			return fmt.Errorf("sinks.nas.url / username / password 必填")
		}
		if n.RootPath == "" {
			return fmt.Errorf("sinks.nas.root_path 必填")
		}
		if n.UserDirs.Enabled && (n.UserDirs.FastGPTBaseURL == "" || n.UserDirs.FastGPTAPIKey == "") {
			return fmt.Errorf("sinks.nas.user_dirs 启用时 fastgpt_base_url / fastgpt_api_key 必填")
		}
	}

	// FastGPT sink
	if g := &c.Sinks.FastGPT; g.Enabled {
		if g.BaseURL == "" || g.APIKey == "" {
			return fmt.Errorf("sinks.fastgpt.base_url / api_key 必填")
		}
		if g.Interval.D() <= 0 {
			g.Interval = Duration(5 * time.Minute)
		}
	}

	// 至少启用一个出口
	if !c.Sinks.NAS.Enabled && !c.Sinks.FastGPT.Enabled {
		return fmt.Errorf("sinks.nas 与 sinks.fastgpt 至少启用一个")
	}

	// 分类器默认值
	c.Classifier.ApplyDefaults()
	return nil
}
