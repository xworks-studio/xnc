//go:build windows

package display

// 开发机探针：SwDeviceCreate 参数合法性不依赖驱动入 store——参数错误
// 直接 E_INVALIDARG，参数合法则进入设备创建流程（无驱动时回调
// CreateResult = 驱动未找到类错误）。手动运行：go test -run TestSwDeviceArgsProbe -v
import "testing"

func TestSwDeviceArgsProbe(t *testing.T) {
	h, err := swDeviceCreate()
	t.Logf("swDeviceCreate -> handle=0x%x err=%v", h, err)
	if err != nil {
		// 预期形态（无驱动）："display: SwDeviceCreate device creation failed 0x..."；
		// 参数错误则是 "display: SwDeviceCreate failed 0x80070057"。
		t.Logf("probe error class: %v", err)
	}
	if h != 0 {
		swDeviceCloseHandle(h)
	}
}
