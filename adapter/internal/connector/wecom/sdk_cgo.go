//go:build cgo

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
	"fmt"
	"unsafe"
)

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

func (s *cgoSDK) GetChatData(seq int64, limit int) (*ChatDataResponse, error) {
	out := C.NewSlice() // SDK 分配，FreeSlice 配对释放
	if out == nil {
		return nil, fmt.Errorf("NewSlice 返回空")
	}
	defer C.FreeSlice(out)

	ret := C.GetChatData(s.sdk, C.ulonglong(seq), C.uint(limit), nil, nil, 10, out)
	if ret != 0 {
		return nil, fmt.Errorf("GetChatData ret=%d", int(ret))
	}
	jsonStr := sliceToString(out)
	if jsonStr == "" {
		return nil, fmt.Errorf("GetChatData 返回空")
	}
	var resp ChatDataResponse
	if err := jsonUnmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, fmt.Errorf("解析 chatdata 失败: %w", err)
	}
	return &resp, nil
}

func (s *cgoSDK) GetMediaData(sdkFileID, indexBuf string) ([]byte, string, bool, error) {
	var md C.MediaData_t // 无 NewMediaData：结构体由调用方分配，SDK 只填内部指针
	cFileID := C.CString(sdkFileID)
	cIndex := C.CString(indexBuf)
	defer C.free(unsafe.Pointer(cFileID))
	defer C.free(unsafe.Pointer(cIndex))

	ret := C.GetMediaData(s.sdk, cIndex, cFileID, nil, nil, 10, &md)
	if ret != 0 {
		return nil, indexBuf, false, fmt.Errorf("GetMediaData ret=%d", int(ret))
	}
	defer C.FreeMediaData(&md) // 释放 SDK 内部分配的 data/outindexbuf

	var data []byte
	if md.data != nil && md.data_len > 0 {
		data = C.GoBytes(unsafe.Pointer(md.data), md.data_len)
	}
	nextIndex := ""
	if md.outindexbuf != nil && md.out_len > 0 {
		nextIndex = C.GoStringN(md.outindexbuf, md.out_len)
	}
	isFinish := md.is_finish == 1
	return data, nextIndex, isFinish, nil
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
