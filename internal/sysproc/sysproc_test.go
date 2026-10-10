package sysproc

import (
	"os/exec"
	"runtime"
	"testing"
)

func TestHideWindow(t *testing.T) {
	cmd := exec.Command("echo", "test")
	res := HideWindow(cmd)
	if res == nil {
		t.Fatal("HideWindow returned nil")
	}

	if runtime.GOOS == "windows" {
		if res.SysProcAttr == nil {
			t.Fatal("SysProcAttr is nil on windows")
		}
		if !res.SysProcAttr.HideWindow {
			t.Errorf("HideWindow expected true, got false")
		}
		if res.SysProcAttr.CreationFlags&0x08000000 == 0 {
			t.Errorf("CREATE_NO_WINDOW flag not set on windows: %x", res.SysProcAttr.CreationFlags)
		}
	}
}

func TestHideWindowNil(t *testing.T) {
	if got := HideWindow(nil); got != nil {
		t.Errorf("HideWindow(nil) = %v, want nil", got)
	}
}
