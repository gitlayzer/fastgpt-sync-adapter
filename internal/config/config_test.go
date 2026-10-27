package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStreamlinedConfigLoad(t *testing.T) {
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "config.yaml")

	minimalYAML := `
server:
  addr: ":8080"
  token: "test-token"

auth:
  enabled: true
  addr: ":8089"

sinks:
  nas:
    enabled: true
    url: "https://192.168.10.204:5001"
    username: "FastGPT"
    password: "Password"
    root_path: "/FastGPT-test"

connectors:
  wecom:
    enabled: true
    corp_id: "ww_test"
    secret: "sec_test"
    rsa_private_key_file: "./private.pem"
  feishu:
    enabled: true
    app_id: "cli_test"
    app_secret: "sec_test"

identity:
  source: bitable
  bitable:
    app_token: "app_tok"
    table_id: "tbl_id"
    app_id: "cli_test"
    app_secret: "sec_test"
`
	if err := os.WriteFile(confPath, []byte(minimalYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(confPath)
	if err != nil {
		t.Fatalf("Load streamlined config failed: %v", err)
	}

	// 验证默认值自动填充
	if cfg.Engine.StateFile == "" || !filepath.IsAbs(cfg.Engine.StateFile) {
		t.Fatalf("StateFile 默认值未自动生成: %s", cfg.Engine.StateFile)
	}
	if cfg.Storage.DBFile == "" || !filepath.IsAbs(cfg.Storage.DBFile) {
		t.Fatalf("DBFile 默认值未自动生成: %s", cfg.Storage.DBFile)
	}
	if cfg.Connectors.WeCom.BatchLimit != 100 {
		t.Fatalf("WeCom BatchLimit 默认值不对: %d", cfg.Connectors.WeCom.BatchLimit)
	}
	if cfg.Connectors.Feishu.BatchLimit != 50 {
		t.Fatalf("Feishu BatchLimit 默认值不对: %d", cfg.Connectors.Feishu.BatchLimit)
	}
	if cfg.Connectors.Feishu.Mode != "both" {
		t.Fatalf("Feishu Mode 默认值不对: %s", cfg.Connectors.Feishu.Mode)
	}
	if cfg.Sinks.NAS.MaxFileSizeMB != 0 && cfg.Sinks.NAS.MaxFileSizeMB != 200 {
		t.Fatalf("NAS MaxFileSizeMB 异常: %d", cfg.Sinks.NAS.MaxFileSizeMB)
	}
	if len(cfg.Classifier.Rules) == 0 {
		t.Fatalf("Classifier 规则默认值未填充")
	}
}
