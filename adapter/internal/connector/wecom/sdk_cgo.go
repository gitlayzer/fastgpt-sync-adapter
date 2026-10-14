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
extern void FreeSlice(Slice_t *slice);
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
// 动态库通过 #cgo -L 在编译期定位（默认 adapter/lib），运行时用 LD_LIBRARY_PATH 加载。
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

// sliceToString 把 C Slice_t 转为 Go string 并释放。
func sliceToString(s *C.Slice_t) string {
	if s == nil || s.buf == nil {
		return ""
	}
	str := C.GoStringN(s.buf, s.len)
	C.FreeSlice(s)
	return str
}

func (s *cgoSDK) GetChatData(seq int64, limit int) (*ChatDataResponse, error) {
	var out C.Slice_t
	ret := C.GetChatData(s.sdk, C.ulonglong(seq), C.uint(limit), nil, nil, 10, &out)
	if ret != 0 {
		return nil, fmt.Errorf("GetChatData ret=%d", int(ret))
	}
	jsonStr := sliceToString(&out)
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
	var md C.MediaData_t
	cFileID := C.CString(sdkFileID)
	cIndex := C.CString(indexBuf)
	defer C.free(unsafe.Pointer(cFileID))
	defer C.free(unsafe.Pointer(cIndex))

	ret := C.GetMediaData(s.sdk, cIndex, cFileID, nil, nil, 10, &md)
	if ret != 0 {
		return nil, indexBuf, false, fmt.Errorf("GetMediaData ret=%d", int(ret))
	}
	// data 是原始字节
	var data []byte
	if md.data != nil && md.data_len > 0 {
		data = C.GoBytes(unsafe.Pointer(md.data), md.data_len)
	}
	nextIndex := ""
	if md.outindexbuf != nil && md.out_len > 0 {
		nextIndex = C.GoStringN(md.outindexbuf, md.out_len)
	}
	isFinish := md.is_finish == 1
	// 释放 C 端内存
	if md.data != nil {
		C.free(unsafe.Pointer(md.data))
	}
	if md.outindexbuf != nil {
		C.free(unsafe.Pointer(md.outindexbuf))
	}
	return data, nextIndex, isFinish, nil
}

// DecryptData 用 SDK 内置解密（无需自己实现 RSA/AES）。
func (s *cgoSDK) DecryptData(encryptRandomKey, encryptChatMsg string) (string, error) {
	var out C.Slice_t
	cKey := C.CString(encryptRandomKey)
	cMsg := C.CString(encryptChatMsg)
	defer C.free(unsafe.Pointer(cKey))
	defer C.free(unsafe.Pointer(cMsg))

	ret := C.DecryptData(cKey, cMsg, &out)
	if ret != 0 {
		return "", fmt.Errorf("DecryptData ret=%d", int(ret))
	}
	return sliceToString(&out), nil
}
