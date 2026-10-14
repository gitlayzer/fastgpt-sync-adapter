// Package config 配置加载。结构按「引擎 / 连接器 / Sink」三组组织：
// connectors 下每个数据源一节（enabled 开关 + 各自凭证），sinks 下每个出口一节。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"fastgpt-sync-adapter/adapter/internal/connector/wecom"
	nassink "fastgpt-sync-adapter/adapter/internal/sink/nas"
	fastgptsink "fastgpt-sync-adapter/adapter/internal/sink/fastgpt"
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
	// 退化为整数纳秒
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
		// feishu: TODO
	} `yaml:"connectors"`

	Sinks struct {
		NAS     NASSinkConfig     `yaml:"nas"`
		FastGPT FastGPTSinkConfig `yaml:"fastgpt"`
	} `yaml:"sinks"`
}

// EngineConfig 引擎全局配置。
type EngineConfig struct {
	StateFile  string   `yaml:"state_file"`  // 各连接器 checkpoint 统一存放
	RunOnStart bool     `yaml:"run_on_start"`
}

// WeComConnectorConfig 企微连接器配置节。
type WeComConnectorConfig struct {
	Enabled    bool         `yaml:"enabled"`
	Interval   Duration     `yaml:"interval"`     // 拉取间隔（需高于媒体附件 3 天保留期，建议分钟级）
	BatchLimit int          `yaml:"batch_limit"`  // 单批条数，默认 100（SDK 上限 1000）
	WeCom      wecom.Config `yaml:",inline"`      // corp_id/secret/rsa_private_key(_file)/sdk_lib_path
}

// NASSinkConfig NAS Sink 配置节。
type NASSinkConfig struct {
	Enabled bool `yaml:"enabled"`
	nassink.Config `yaml:",inline"`
}

// FastGPTSinkConfig FastGPT 上传 Sink（方案 A）配置节。
type FastGPTSinkConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"` // 扫描上传间隔
	fastgptsink.Config `yaml:",inline"`
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

	// NAS Sink
	if n := &c.Sinks.NAS; n.Enabled {
		if n.URL == "" || n.Username == "" || n.Password == "" {
			return fmt.Errorf("sinks.nas.url / username / password 必填")
		}
		if n.RootPath == "" {
			return fmt.Errorf("sinks.nas.root_path 必填")
		}
	}

	// FastGPT 上传 Sink
	if f := &c.Sinks.FastGPT; f.Enabled {
		if f.BaseURL == "" || f.APIKey == "" || f.DatasetID == "" {
			return fmt.Errorf("sinks.fastgpt.base_url / api_key / dataset_id 必填")
		}
		if f.WatchPath == "" {
			return fmt.Errorf("sinks.fastgpt.watch_path 必填（要扫描上传的目录）")
		}
		if f.Interval.D() <= 0 {
			f.Interval = Duration(10 * time.Minute)
		}
		if f.StateFile == "" {
			f.StateFile = "./fastgpt_upload_state.json"
		}
	}
	return nil
}
