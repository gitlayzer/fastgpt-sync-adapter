package wecom

import (
	"encoding/base64"
	"encoding/json"
)

// jsonUnmarshal 辅助：解析 JSON。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// b64Decode 辅助：base64 解码。
func b64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
