//go:build !cgo || !linux

package wecom

import "fmt"

// 非 cgo 环境的占位实现：没有 SDK 动态库时给出明确提示。
type noopSDK struct{}

func NewSDKFromLib(corpID, secret, libPath string) (SDK, error) {
	return nil, fmt.Errorf("企微会话存档需要 cgo + libWeWorkFinanceSdk_C 动态库；当前以 CGO_ENABLED=0 编译，无法加载。请用 cgo 编译并将 SDK 库放到 adapter/lib 或配置 wecom.sdk_lib_path")
}
