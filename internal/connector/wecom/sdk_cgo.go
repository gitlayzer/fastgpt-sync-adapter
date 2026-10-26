//go:build cgo && linux

package wecom

/*
#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/../../../lib/amd64
#cgo linux,arm64 LDFLAGS: -L${SRCDIR}/../../../lib/arm64
#cgo LDFLAGS: -lWeWorkFinanceSdk_C
#cgo linux LDFLAGS: -lstdc++ -lm -lpthread
#cgo darwin LDFLAGS: -lstdc++ -lm -lpthread
#include <stdlib.h>
#include <string.h>

typedef struct WeWorkFinanceSdk_t WeWorkFinanceSdk_t;
typedef struct Slice_t { char *buf; int len; } Slice_t;
typedef struct MediaData {
    char *outindexbuf;
    int out_len;
    char *data;
    int data_len;
    int is_finish;
} MediaData_t;

extern WeWorkFinanceSdk_t* NewSdk();
extern int Init(WeWorkFinanceSdk_t *sdk, const char *corpid, const char *secret);
extern int GetChatData(WeWorkFinanceSdk_t *sdk, unsigned long long seq, unsigned int limit,
                       const char *proxy, const char *passwd, int timeout, Slice_t *chatDatas);
extern int DecryptData(const char *encrypt_key, const char *encrypt_msg, Slice_t *msg);
extern int GetMediaData(WeWorkFinanceSdk_t *sdk, const char *indexbuf, const char *sdkFileid,
                        const char *proxy, const char *passwd, int timeout, MediaData_t *media_data);
extern void DestroySdk(WeWorkFinanceSdk_t *sdk);

// Slice/MediaData 的分配释放必须走 SDK 自己的函数（FreeSlice 会连结构体一起释放，
// 对 Go 栈变量调用会 double free 崩溃——已实测）。
extern Slice_t *NewSlice();
extern void FreeSlice(Slice_t *slice);
extern char *GetContentFromSlice(Slice_t *slice);
extern int GetSliceLen(Slice_t *slice);
extern void FreeMediaData(MediaData_t *media_data);
*/
import "C"
import (
	"context"
	"fmt"
	"log"
	"time"
	"unsafe"
)

// sleepCtx 可被取消的等待。返回 false 表示 ctx 已取消。
// 单次 C 调用无法中断（SDK 是同步阻塞的），但至少让重试之间的等待能响应退出。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// GetChatData 的超时与重试参数。
//
// ret=10001 是 SDK 的网络层错误（头文件：10001 网络请求失败），和业务错误不同 ——
// 它通常是瞬时的（企微服务端抖动、连接复用失效、本机网络切换）。
//
// 实测数据（2026-10-22，同一 fileid 连续调用）：
//   - GetChatData 失败率较低，重试 1~2 次即可恢复
//   - GetMediaData 失败率约 60%，且与超时值无关（失败在 2~3.5s 内返回，
//     远小于 20s 超时），同一 fileid 的成败呈随机交替
//
// 因此媒体下载需要比消息拉取多得多的重试次数：
// 按 60% 失败率、10 次尝试计算，单片最终失败概率约 0.6%，可接受。
//
// 注意：这是「单片」的重试，重试只在这一层做。上层 FetchAttachment 只负责
// 分片续传，不再叠加退避，否则最坏情况会变成 10×N 次调用把整批卡死。
const (
	chatDataTimeoutSec = 20 // GetChatData 单次超时
	mediaTimeoutSec    = 30 // GetMediaData 单次超时（大文件分片需要更久）

	// 两者失败率相近（企微 SDK 网络层不稳定），都给足重试次数。
	// 实测在容器环境里 GetChatData 会连续 3 次 10001，3 次上限不够用。
	chatDataAttempts = 8
	mediaAttempts    = 10
)

// retNetworkErr 是 SDK 的「网络请求失败」错误码，值得重试。
const retNetworkErr = 10001

// cgoSDK 通过 cgo 调用企微官方 C SDK（动态库 libWeWorkFinanceSdk_C.so）。
type cgoSDK struct {
	sdk *C.WeWorkFinanceSdk_t
}

// NewSDKFromLib 初始化 C SDK。
// 动态库通过 #cgo -L 在编译期定位（按 GOARCH 选 adapter/lib/amd64|arm64），运行时用 LD_LIBRARY_PATH 加载。
func NewSDKFromLib(corpID, secret, libPath string) (SDK, error) {
	sdk := C.NewSdk()
	if sdk == nil {
		return nil, fmt.Errorf("NewSdk 返回空")
	}
	cCorp := C.CString(corpID)
	cSecret := C.CString(secret)
	defer C.free(unsafe.Pointer(cCorp))
	defer C.free(unsafe.Pointer(cSecret))

	ret := C.Init(sdk, cCorp, cSecret)
	if ret != 0 {
		C.DestroySdk(sdk)
		return nil, fmt.Errorf("企微 SDK Init 失败 ret=%d（检查 corpid/secret）", int(ret))
	}
	return &cgoSDK{sdk: sdk}, nil
}

// sliceToString 读取 Slice 内容（不释放，释放由调用方用 FreeSlice 完成）。
func sliceToString(s *C.Slice_t) string {
	if s == nil {
		return ""
	}
	buf := C.GetContentFromSlice(s)
	n := C.GetSliceLen(s)
	if buf == nil || n <= 0 {
		return ""
	}
	return C.GoStringN(buf, n)
}

func (s *cgoSDK) GetChatData(ctx context.Context, seq int64, limit int) (*ChatDataResponse, error) {
	var lastRet int
	for i := 0; i < chatDataAttempts; i++ {
		if i > 0 {
			wait := time.Duration(i) * 2 * time.Second
			log.Printf("[wecom] GetChatData 重试 %d/%d，等待 %s（上次 ret=%d）",
				i+1, chatDataAttempts, wait, lastRet)
			if !sleepCtx(ctx, wait) {
				return nil, ctx.Err()
			}
		}

		// 每次尝试都新建 Slice：失败时 SDK 可能已写入部分内容，复用有风险
		out := C.NewSlice() // SDK 分配，FreeSlice 配对释放
		if out == nil {
			return nil, fmt.Errorf("NewSlice 返回空")
		}
		ret := int(C.GetChatData(s.sdk, C.ulonglong(seq), C.uint(limit), nil, nil,
			C.int(chatDataTimeoutSec), out))
		if ret != 0 {
			C.FreeSlice(out)
			lastRet = ret
			// 只有网络层错误值得重试；其它（如 10009 IP 非法、10011 证书失效）
			// 是确定性错误，重试只会拖慢失败
			if ret != retNetworkErr {
				return nil, fmt.Errorf("GetChatData ret=%d（%s）", ret, sdkErrHint(ret))
			}
			continue
		}
		jsonStr := sliceToString(out)
		C.FreeSlice(out)
		if jsonStr == "" {
			return nil, fmt.Errorf("GetChatData 返回空")
		}
		var resp ChatDataResponse
		if err := jsonUnmarshal([]byte(jsonStr), &resp); err != nil {
			return nil, fmt.Errorf("解析 chatdata 失败: %w", err)
		}
		return &resp, nil
	}
	return nil, fmt.Errorf("GetChatData ret=%d（已重试 %d 次，%s）", lastRet, chatDataAttempts, sdkErrHint(lastRet))
}

// sdkErrHint 把 SDK 错误码翻译成可读原因（依据 lib/WeWorkFinanceSdk_C.h 的注释）。
func sdkErrHint(ret int) string {
	switch ret {
	case 10000:
		return "请求参数错误"
	case 10001:
		return "网络请求失败"
	case 10002:
		return "数据解析失败"
	case 10003:
		return "系统失败"
	case 10004:
		return "私钥解密失败"
	case 10005:
		return "fileid 无效"
	case 10006:
		return "解密失败"
	case 10007:
		return "找不到消息加密版本的私钥，需更新私钥"
	case 10008:
		return "encrypt_key 解析失败"
	case 10009:
		return "IP 非法（检查企微白名单）"
	case 10010:
		return "数据过期"
	case 10011:
		return "证书失效"
	}
	return "未知错误"
}

func (s *cgoSDK) GetMediaData(ctx context.Context, sdkFileID, indexBuf string) ([]byte, string, bool, error) {
	cFileID := C.CString(sdkFileID)
	cIndex := C.CString(indexBuf)
	defer C.free(unsafe.Pointer(cFileID))
	defer C.free(unsafe.Pointer(cIndex))

	var lastRet int
	for i := 0; i < mediaAttempts; i++ {
		if i > 0 {
			// 失败在 2~3.5s 内就返回，说明不是超时，无需长退避；
			// 固定 500ms 间隔快速重试，避免单个附件卡住整批
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return nil, indexBuf, false, ctx.Err()
			}
			// 只在第 2、5、10 次时打日志，避免高失败率下刷屏
			if i == 1 || i == 4 || i == mediaAttempts-1 {
				log.Printf("[wecom] GetMediaData 重试 %d/%d（上次 ret=%d）",
					i+1, mediaAttempts, lastRet)
			}
		}

		// MediaData_t 必须在 C 堆分配：FreeMediaData 会连结构体一起释放（同 Slice 的坑，已实测）
		md := (*C.MediaData_t)(C.malloc(C.size_t(unsafe.Sizeof(C.MediaData_t{}))))
		if md == nil {
			return nil, indexBuf, false, fmt.Errorf("malloc MediaData_t 失败")
		}
		*md = C.MediaData_t{}

		ret := int(C.GetMediaData(s.sdk, cIndex, cFileID, nil, nil,
			C.int(mediaTimeoutSec), md))
		if ret != 0 {
			C.FreeMediaData(md)
			lastRet = ret
			// 10010 数据过期、10005 fileid 无效属于确定性失败，重试无益
			if ret != retNetworkErr {
				return nil, indexBuf, false, fmt.Errorf("GetMediaData ret=%d（%s）", ret, sdkErrHint(ret))
			}
			continue
		}

		var data []byte
		if md.data != nil && md.data_len > 0 {
			data = C.GoBytes(unsafe.Pointer(md.data), md.data_len)
		}
		nextIndex := ""
		if md.outindexbuf != nil && md.out_len > 0 {
			nextIndex = C.GoStringN(md.outindexbuf, md.out_len)
		}
		isFinish := md.is_finish == 1
		C.FreeMediaData(md) // 释放 SDK 内部分配的 data/outindexbuf 及结构体本身
		return data, nextIndex, isFinish, nil
	}
	return nil, indexBuf, false, fmt.Errorf("GetMediaData ret=%d（已重试 %d 次，%s）",
		lastRet, mediaAttempts, sdkErrHint(lastRet))
}

// DecryptData 用 SDK 内置解密（无需自己实现 RSA/AES）。
func (s *cgoSDK) DecryptData(encryptRandomKey, encryptChatMsg string) (string, error) {
	out := C.NewSlice()
	if out == nil {
		return "", fmt.Errorf("NewSlice 返回空")
	}
	defer C.FreeSlice(out)

	cKey := C.CString(encryptRandomKey)
	cMsg := C.CString(encryptChatMsg)
	defer C.free(unsafe.Pointer(cKey))
	defer C.free(unsafe.Pointer(cMsg))

	ret := C.DecryptData(cKey, cMsg, out)
	if ret != 0 {
		return "", fmt.Errorf("DecryptData ret=%d", int(ret))
	}
	return sliceToString(out), nil
}

// Close 释放 SDK 句柄。
func (s *cgoSDK) Close() error {
	if s.sdk != nil {
		C.DestroySdk(s.sdk)
		s.sdk = nil
	}
	return nil
}
