//go:build windows

package mono

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Name is the clock as hello names it to a plugin (shared_clock): the plugin reads the same one, or says it cannot.
const Name = "qpc"

// QueryPerformanceCounter is one counter for the whole machine, counting since boot at a frequency fixed at boot, which
// a step of the wall clock does not move: every process reads the same value (Microsoft: "Acquiring high-resolution
// time stamps"). Read raw, from kernel32 in System32.
var (
	procQPC = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	procQPF = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	qpcFreq = func() int64 {
		var f int64
		if procQPF.Find() != nil {
			return 0
		}
		if r, _, _ := procQPF.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
			return 0
		}
		return f
	}()
)

// read is QueryPerformanceCounter in nanoseconds (qpcNS), or ok false when it cannot be read.
func read() (int64, bool) {
	if qpcFreq <= 0 || procQPC.Find() != nil {
		return 0, false
	}
	var t int64
	if r, _, _ := procQPC.Call(uintptr(unsafe.Pointer(&t))); r == 0 {
		return 0, false
	}
	return qpcNS(t, qpcFreq), true
}
