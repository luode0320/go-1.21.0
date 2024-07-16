// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/atomic"
	"unsafe"
)

// TODO(brainman): should not need those
const (
	_NSIG = 65
)

//go:cgo_import_dynamic runtime._AddVectoredExceptionHandler AddVectoredExceptionHandler%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._CloseHandle CloseHandle%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._CreateEventA CreateEventA%4 "kernel32.dll"
//go:cgo_import_dynamic runtime._CreateFileA CreateFileA%7 "kernel32.dll"
//go:cgo_import_dynamic runtime._CreateIoCompletionPort CreateIoCompletionPort%4 "kernel32.dll"
//go:cgo_import_dynamic runtime._CreateThread CreateThread%6 "kernel32.dll"
//go:cgo_import_dynamic runtime._CreateWaitableTimerA CreateWaitableTimerA%3 "kernel32.dll"
//go:cgo_import_dynamic runtime._CreateWaitableTimerExW CreateWaitableTimerExW%4 "kernel32.dll"
//go:cgo_import_dynamic runtime._DuplicateHandle DuplicateHandle%7 "kernel32.dll"
//go:cgo_import_dynamic runtime._ExitProcess ExitProcess%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._FreeEnvironmentStringsW FreeEnvironmentStringsW%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetConsoleMode GetConsoleMode%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetCurrentThreadId GetCurrentThreadId%0 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetEnvironmentStringsW GetEnvironmentStringsW%0 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetErrorMode GetErrorMode%0 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetProcAddress GetProcAddress%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetProcessAffinityMask GetProcessAffinityMask%3 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetQueuedCompletionStatusEx GetQueuedCompletionStatusEx%6 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetStdHandle GetStdHandle%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetSystemDirectoryA GetSystemDirectoryA%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetSystemInfo GetSystemInfo%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._GetThreadContext GetThreadContext%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetThreadContext SetThreadContext%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._LoadLibraryExW LoadLibraryExW%3 "kernel32.dll"
//go:cgo_import_dynamic runtime._LoadLibraryW LoadLibraryW%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._PostQueuedCompletionStatus PostQueuedCompletionStatus%4 "kernel32.dll"
//go:cgo_import_dynamic runtime._RaiseFailFastException RaiseFailFastException%3 "kernel32.dll"
//go:cgo_import_dynamic runtime._ResumeThread ResumeThread%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetConsoleCtrlHandler SetConsoleCtrlHandler%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetErrorMode SetErrorMode%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetEvent SetEvent%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetProcessPriorityBoost SetProcessPriorityBoost%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetThreadPriority SetThreadPriority%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetUnhandledExceptionFilter SetUnhandledExceptionFilter%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._SetWaitableTimer SetWaitableTimer%6 "kernel32.dll"
//go:cgo_import_dynamic runtime._SuspendThread SuspendThread%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._SwitchToThread SwitchToThread%0 "kernel32.dll"
//go:cgo_import_dynamic runtime._TlsAlloc TlsAlloc%0 "kernel32.dll"
//go:cgo_import_dynamic runtime._VirtualAlloc VirtualAlloc%4 "kernel32.dll"
//go:cgo_import_dynamic runtime._VirtualFree VirtualFree%3 "kernel32.dll"
//go:cgo_import_dynamic runtime._VirtualQuery VirtualQuery%3 "kernel32.dll"
//go:cgo_import_dynamic runtime._WaitForSingleObject WaitForSingleObject%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._WaitForMultipleObjects WaitForMultipleObjects%4 "kernel32.dll"
//go:cgo_import_dynamic runtime._WerGetFlags WerGetFlags%2 "kernel32.dll"
//go:cgo_import_dynamic runtime._WerSetFlags WerSetFlags%1 "kernel32.dll"
//go:cgo_import_dynamic runtime._WriteConsoleW WriteConsoleW%5 "kernel32.dll"
//go:cgo_import_dynamic runtime._WriteFile WriteFile%5 "kernel32.dll"

type stdFunction unsafe.Pointer

var (
	// Following syscalls are available on every Windows PC.
	// All these variables are set by the Windows executable
	// loader before the Go program starts.
	_AddVectoredExceptionHandler,
	_CloseHandle,
	_CreateEventA,
	_CreateFileA,
	_CreateIoCompletionPort,
	_CreateThread,
	_CreateWaitableTimerA,
	_CreateWaitableTimerExW,
	_DuplicateHandle,
	_ExitProcess,
	_FreeEnvironmentStringsW,
	_GetConsoleMode,
	_GetCurrentThreadId,
	_GetEnvironmentStringsW,
	_GetErrorMode,
	_GetProcAddress,
	_GetProcessAffinityMask,
	_GetQueuedCompletionStatusEx,
	_GetStdHandle,
	_GetSystemDirectoryA,
	_GetSystemInfo,
	_GetSystemTimeAsFileTime,
	_GetThreadContext,
	_SetThreadContext,
	_LoadLibraryExW,
	_LoadLibraryW,
	_PostQueuedCompletionStatus,
	_QueryPerformanceCounter,
	_QueryPerformanceFrequency,
	_RaiseFailFastException,
	_ResumeThread,
	_SetConsoleCtrlHandler,
	_SetErrorMode,
	_SetEvent,
	_SetProcessPriorityBoost,
	_SetThreadPriority,
	_SetUnhandledExceptionFilter,
	_SetWaitableTimer,
	_SuspendThread,
	_SwitchToThread,
	_TlsAlloc,
	_VirtualAlloc,
	_VirtualFree,
	_VirtualQuery,
	_WaitForSingleObject,
	_WaitForMultipleObjects,
	_WerGetFlags,
	_WerSetFlags,
	_WriteConsoleW,
	_WriteFile,
	_ stdFunction

	// Following syscalls are only available on some Windows PCs.
	// We will load syscalls, if available, before using them.
	_AddVectoredContinueHandler,
	_ stdFunction

	// Use ProcessPrng to generate cryptographically random data.
	_ProcessPrng stdFunction

	// Load ntdll.dll manually during startup, otherwise Mingw
	// links wrong printf function to cgo executable (see issue
	// 12030 for details).
	_NtWaitForSingleObject  stdFunction
	_RtlGetCurrentPeb       stdFunction
	_RtlGetNtVersionNumbers stdFunction

	// These are from non-kernel32.dll, so we prefer to LoadLibraryEx them.
	_timeBeginPeriod,
	_timeEndPeriod,
	_WSAGetOverlappedResult,
	_ stdFunction
)

var (
	bcryptprimitivesdll = [...]uint16{'b', 'c', 'r', 'y', 'p', 't', 'p', 'r', 'i', 'm', 'i', 't', 'i', 'v', 'e', 's', '.', 'd', 'l', 'l', 0}
	kernel32dll         = [...]uint16{'k', 'e', 'r', 'n', 'e', 'l', '3', '2', '.', 'd', 'l', 'l', 0}
	ntdlldll            = [...]uint16{'n', 't', 'd', 'l', 'l', '.', 'd', 'l', 'l', 0}
	powrprofdll         = [...]uint16{'p', 'o', 'w', 'r', 'p', 'r', 'o', 'f', '.', 'd', 'l', 'l', 0}
	winmmdll            = [...]uint16{'w', 'i', 'n', 'm', 'm', '.', 'd', 'l', 'l', 0}
	ws2_32dll           = [...]uint16{'w', 's', '2', '_', '3', '2', '.', 'd', 'l', 'l', 0}
)

// Function to be called by windows CreateThread
// to start new os thread.
func tstart_stdcall(newm *m)

// Init-time helper
func wintls()

type mOS struct {
	threadLock mutex   // protects "thread" and prevents closing
	thread     uintptr // thread handle

	waitsema   uintptr // semaphore for parking on locks
	resumesema uintptr // semaphore to indicate suspend/resume

	highResTimer uintptr // high resolution timer handle used in usleep

	// preemptExtLock synchronizes preemptM with entry/exit from
	// external C code.
	//
	// This protects against races between preemptM calling
	// SuspendThread and external code on this thread calling
	// ExitProcess. If these happen concurrently, it's possible to
	// exit the suspending thread and suspend the exiting thread,
	// leading to deadlock.
	//
	// 0 indicates this M is not being preempted or in external
	// code. Entering external code CASes this from 0 to 1. If
	// this fails, a preemption is in progress, so the thread must
	// wait for the preemption. preemptM also CASes this from 0 to
	// 1. If this fails, the preemption fails (as it would if the
	// PC weren't in Go code). The value is reset to 0 when
	// returning from external code or after a preemption is
	// complete.
	//
	// TODO(austin): We may not need this if preemption were more
	// tightly synchronized on the G/P status and preemption
	// blocked transition into _Gsyscall/_Psyscall.
	preemptExtLock uint32
}

// Stubs so tests can link correctly. These should never be called.
func open(name *byte, mode, perm int32) int32 {
	throw("unimplemented")
	return -1
}
func closefd(fd int32) int32 {
	throw("unimplemented")
	return -1
}
func read(fd int32, p unsafe.Pointer, n int32) int32 {
	throw("unimplemented")
	return -1
}

type sigset struct{}

// Call a Windows function with stdcall conventions,
// and switch to os stack during the call.
func asmstdcall(fn unsafe.Pointer)

var asmstdcallAddr unsafe.Pointer

func windowsFindfunc(lib uintptr, name []byte) stdFunction {
	if name[len(name)-1] != 0 {
		throw("usage")
	}
	f := stdcall2(_GetProcAddress, lib, uintptr(unsafe.Pointer(&name[0])))
	return stdFunction(unsafe.Pointer(f))
}

const _MAX_PATH = 260 // https://docs.microsoft.com/en-us/windows/win32/fileio/maximum-file-path-limitation
var sysDirectory [_MAX_PATH + 1]byte
var sysDirectoryLen uintptr

func initSysDirectory() {
	l := stdcall2(_GetSystemDirectoryA, uintptr(unsafe.Pointer(&sysDirectory[0])), uintptr(len(sysDirectory)-1))
	if l == 0 || l > uintptr(len(sysDirectory)-1) {
		throw("Unable to determine system directory")
	}
	sysDirectory[l] = '\\'
	sysDirectoryLen = l + 1
}

func windowsLoadSystemLib(name []uint16) uintptr {
	return stdcall3(_LoadLibraryExW, uintptr(unsafe.Pointer(&name[0])), 0, _LOAD_LIBRARY_SEARCH_SYSTEM32)
}

const haveCputicksAsm = GOARCH == "386" || GOARCH == "amd64"

func loadOptionalSyscalls() {
	k32 := windowsLoadSystemLib(kernel32dll[:])
	if k32 == 0 {
		throw("kernel32.dll not found")
	}
	_AddVectoredContinueHandler = windowsFindfunc(k32, []byte("AddVectoredContinueHandler\000"))

	bcryptPrimitives := windowsLoadSystemLib(bcryptprimitivesdll[:])
	if bcryptPrimitives == 0 {
		throw("bcryptprimitives.dll not found")
	}
	_ProcessPrng = windowsFindfunc(bcryptPrimitives, []byte("ProcessPrng\000"))

	n32 := windowsLoadSystemLib(ntdlldll[:])
	if n32 == 0 {
		throw("ntdll.dll not found")
	}
	_NtWaitForSingleObject = windowsFindfunc(n32, []byte("NtWaitForSingleObject\000"))
	_RtlGetCurrentPeb = windowsFindfunc(n32, []byte("RtlGetCurrentPeb\000"))
	_RtlGetNtVersionNumbers = windowsFindfunc(n32, []byte("RtlGetNtVersionNumbers\000"))

	if !haveCputicksAsm {
		_QueryPerformanceCounter = windowsFindfunc(k32, []byte("QueryPerformanceCounter\000"))
		if _QueryPerformanceCounter == nil {
			throw("could not find QPC syscalls")
		}
	}

	m32 := windowsLoadSystemLib(winmmdll[:])
	if m32 == 0 {
		throw("winmm.dll not found")
	}
	_timeBeginPeriod = windowsFindfunc(m32, []byte("timeBeginPeriod\000"))
	_timeEndPeriod = windowsFindfunc(m32, []byte("timeEndPeriod\000"))
	if _timeBeginPeriod == nil || _timeEndPeriod == nil {
		throw("timeBegin/EndPeriod not found")
	}

	ws232 := windowsLoadSystemLib(ws2_32dll[:])
	if ws232 == 0 {
		throw("ws2_32.dll not found")
	}
	_WSAGetOverlappedResult = windowsFindfunc(ws232, []byte("WSAGetOverlappedResult\000"))
	if _WSAGetOverlappedResult == nil {
		throw("WSAGetOverlappedResult not found")
	}

	if windowsFindfunc(n32, []byte("wine_get_version\000")) != nil {
		// running on Wine
		initWine(k32)
	}
}

func monitorSuspendResume() {
	const (
		_DEVICE_NOTIFY_CALLBACK = 2
	)
	type _DEVICE_NOTIFY_SUBSCRIBE_PARAMETERS struct {
		callback uintptr
		context  uintptr
	}

	powrprof := windowsLoadSystemLib(powrprofdll[:])
	if powrprof == 0 {
		return // Running on Windows 7, where we don't need it anyway.
	}
	powerRegisterSuspendResumeNotification := windowsFindfunc(powrprof, []byte("PowerRegisterSuspendResumeNotification\000"))
	if powerRegisterSuspendResumeNotification == nil {
		return // Running on Windows 7, where we don't need it anyway.
	}
	var fn any = func(context uintptr, changeType uint32, setting uintptr) uintptr {
		for mp := (*m)(atomic.Loadp(unsafe.Pointer(&allm))); mp != nil; mp = mp.alllink {
			if mp.resumesema != 0 {
				stdcall1(_SetEvent, mp.resumesema)
			}
		}
		return 0
	}
	params := _DEVICE_NOTIFY_SUBSCRIBE_PARAMETERS{
		callback: compileCallback(*efaceOf(&fn), true),
	}
	handle := uintptr(0)
	stdcall3(powerRegisterSuspendResumeNotification, _DEVICE_NOTIFY_CALLBACK,
		uintptr(unsafe.Pointer(&params)), uintptr(unsafe.Pointer(&handle)))
}

//go:nosplit
func getLoadLibrary() uintptr {
	return uintptr(unsafe.Pointer(_LoadLibraryW))
}

//go:nosplit
func getLoadLibraryEx() uintptr {
	return uintptr(unsafe.Pointer(_LoadLibraryExW))
}

//go:nosplit
func getGetProcAddress() uintptr {
	return uintptr(unsafe.Pointer(_GetProcAddress))
}

func getproccount() int32 {
	var mask, sysmask uintptr
	ret := stdcall3(_GetProcessAffinityMask, currentProcess, uintptr(unsafe.Pointer(&mask)), uintptr(unsafe.Pointer(&sysmask)))
	if ret != 0 {
		n := 0
		maskbits := int(unsafe.Sizeof(mask) * 8)
		for i := 0; i < maskbits; i++ {
			if mask&(1<<uint(i)) != 0 {
				n++
			}
		}
		if n != 0 {
			return int32(n)
		}
	}
	// use GetSystemInfo if GetProcessAffinityMask fails
	var info systeminfo
	stdcall1(_GetSystemInfo, uintptr(unsafe.Pointer(&info)))
	return int32(info.dwnumberofprocessors)
}

func getPageSize() uintptr {
	var info systeminfo
	stdcall1(_GetSystemInfo, uintptr(unsafe.Pointer(&info)))
	return uintptr(info.dwpagesize)
}

const (
	currentProcess = ^uintptr(0) // -1 = current process
	currentThread  = ^uintptr(1) // -2 = current thread
)

// in sys_windows_386.s and sys_windows_amd64.s:
func getlasterror() uint32

var timeBeginPeriodRetValue uint32

// osRelaxMinNS indicates that sysmon shouldn't osRelax if the next
// timer is less than 60 ms from now. Since osRelaxing may reduce
// timer resolution to 15.6 ms, this keeps timer error under roughly 1
// part in 4.
const osRelaxMinNS = 60 * 1e6

// osRelax is called by the scheduler when transitioning to and from
// all Ps being idle.
//
// Some versions of Windows have high resolution timer. For those
// versions osRelax is noop.
// For Windows versions without high resolution timer, osRelax
// adjusts the system-wide timer resolution. Go needs a
// high resolution timer while running and there's little extra cost
// if we're already using the CPU, but if all Ps are idle there's no
// need to consume extra power to drive the high-res timer.
func osRelax(relax bool) uint32 {
	if haveHighResTimer {
		// If the high resolution timer is available, the runtime uses the timer
		// to sleep for short durations. This means there's no need to adjust
		// the global clock frequency.
		return 0
	}

	if relax {
		return uint32(stdcall1(_timeEndPeriod, 1))
	} else {
		return uint32(stdcall1(_timeBeginPeriod, 1))
	}
}

// haveHighResTimer indicates that the CreateWaitableTimerEx
// CREATE_WAITABLE_TIMER_HIGH_RESOLUTION flag is available.
var haveHighResTimer = false

// createHighResTimer calls CreateWaitableTimerEx with
// CREATE_WAITABLE_TIMER_HIGH_RESOLUTION flag to create high
// resolution timer. createHighResTimer returns new timer
// handle or 0, if CreateWaitableTimerEx failed.
func createHighResTimer() uintptr {
	const (
		// As per @jstarks, see
		// https://github.com/golang/go/issues/8687#issuecomment-656259353
		_CREATE_WAITABLE_TIMER_HIGH_RESOLUTION = 0x00000002

		_SYNCHRONIZE        = 0x00100000
		_TIMER_QUERY_STATE  = 0x0001
		_TIMER_MODIFY_STATE = 0x0002
	)
	return stdcall4(_CreateWaitableTimerExW, 0, 0,
		_CREATE_WAITABLE_TIMER_HIGH_RESOLUTION,
		_SYNCHRONIZE|_TIMER_QUERY_STATE|_TIMER_MODIFY_STATE)
}

func initHighResTimer() {
	h := createHighResTimer()
	if h != 0 {
		haveHighResTimer = true
		stdcall1(_CloseHandle, h)
	}
}

//go:linkname canUseLongPaths os.canUseLongPaths
var canUseLongPaths bool

// We want this to be large enough to hold the contents of sysDirectory, *plus*
// a slash and another component that itself is greater than MAX_PATH.
var longFileName [(_MAX_PATH+1)*2 + 1]byte

// initLongPathSupport initializes the canUseLongPaths variable, which is
// linked into os.canUseLongPaths for determining whether or not long paths
// need to be fixed up. In the best case, this function is running on newer
// Windows 10 builds, which have a bit field member of the PEB called
// "IsLongPathAwareProcess." When this is set, we don't need to go through the
// error-prone fixup function in order to access long paths. So this init
// function first checks the Windows build number, sets the flag, and then
// tests to see if it's actually working. If everything checks out, then
// canUseLongPaths is set to true, and later when called, os.fixLongPath
// returns early without doing work.
func initLongPathSupport() {
	const (
		IsLongPathAwareProcess = 0x80
		PebBitFieldOffset      = 3
		OPEN_EXISTING          = 3
		ERROR_PATH_NOT_FOUND   = 3
	)

	// Check that we're ≥ 10.0.15063.
	var maj, min, build uint32
	stdcall3(_RtlGetNtVersionNumbers, uintptr(unsafe.Pointer(&maj)), uintptr(unsafe.Pointer(&min)), uintptr(unsafe.Pointer(&build)))
	if maj < 10 || (maj == 10 && min == 0 && build&0xffff < 15063) {
		return
	}

	// Set the IsLongPathAwareProcess flag of the PEB's bit field.
	bitField := (*byte)(unsafe.Pointer(stdcall0(_RtlGetCurrentPeb) + PebBitFieldOffset))
	originalBitField := *bitField
	*bitField |= IsLongPathAwareProcess

	// Check that this actually has an effect, by constructing a large file
	// path and seeing whether we get ERROR_PATH_NOT_FOUND, rather than
	// some other error, which would indicate the path is too long, and
	// hence long path support is not successful. This whole section is NOT
	// strictly necessary, but is a nice validity check for the near to
	// medium term, when this functionality is still relatively new in
	// Windows.
	getRandomData(longFileName[len(longFileName)-33 : len(longFileName)-1])
	start := copy(longFileName[:], sysDirectory[:sysDirectoryLen])
	const dig = "0123456789abcdef"
	for i := 0; i < 32; i++ {
		longFileName[start+i*2] = dig[longFileName[len(longFileName)-33+i]>>4]
		longFileName[start+i*2+1] = dig[longFileName[len(longFileName)-33+i]&0xf]
	}
	start += 64
	for i := start; i < len(longFileName)-1; i++ {
		longFileName[i] = 'A'
	}
	stdcall7(_CreateFileA, uintptr(unsafe.Pointer(&longFileName[0])), 0, 0, 0, OPEN_EXISTING, 0, 0)
	// The ERROR_PATH_NOT_FOUND error value is distinct from
	// ERROR_FILE_NOT_FOUND or ERROR_INVALID_NAME, the latter of which we
	// expect here due to the final component being too long.
	if getlasterror() == ERROR_PATH_NOT_FOUND {
		*bitField = originalBitField
		println("runtime: warning: IsLongPathAwareProcess failed to enable long paths; proceeding in fixup mode")
		return
	}

	canUseLongPaths = true
}

// 初始化过程确保了 Go 程序在 Windows 环境下的兼容性和性能，同时处理了一些操作系统级别的细节
// 如错误处理、时间精度、多核利用和内存管理。
func osinit() {
	// 是一个用于调用 Windows 的 stdcall 调用约定的函数地址。
	// 这里使用 unsafe.Pointer 和 abi 包来获取 asmstdcall 函数的地址。
	asmstdcallAddr = unsafe.Pointer(abi.FuncPCABI0(asmstdcall))

	// 加载可选的系统调用。这些调用可能依赖于特定的 Windows 版本或者配置。
	loadOptionalSyscalls()

	// 阻止错误对话框的显示，这通常用于服务器环境，避免出现用户界面。
	preventErrorDialogs()

	// 初始化异常处理程序，用于捕获和处理未捕获的异常。
	initExceptionHandler()

	// 初始化高分辨率定时器，这是为了提高时间精度和性能。
	initHighResTimer()

	// 存储了设置系统定时器分辨率的返回值。
	// 参数 osRelax(false) 表示是否放松定时器分辨率要求。
	timeBeginPeriodRetValue = osRelax(false)

	// 初始化系统目录路径，这有助于确定系统文件的位置。
	initSysDirectory()

	// 初始化长路径支持，以便在 Windows 上使用超过 260 字符的路径。
	initLongPathSupport()

	// 获取系统当前的处理器核心数量。
	ncpu = getproccount()

	// 获取物理页面大小，这对于内存分配和管理至关重要。
	physPageSize = getPageSize()

	// 禁用 Windows 动态优先级提升功能。
	// Windows 的动态优先级提升假设进程有不同的专用线程类型（GUI、I/O、计算等），
	// 但是 Go 进程使用的是等效线程，它们混合执行 GUI、I/O 和计算任务。
	// 在这种情况下，动态优先级提升不仅无效，反而可能有害，所以我们将其关闭。
	// stdcall2 是用于调用 Windows API 的封装函数，_SetProcessPriorityBoost 是对应的 API 名称。
	// 第二个参数是当前进程句柄，第三个参数为 1 表示关闭动态优先级提升。
	stdcall2(_SetProcessPriorityBoost, currentProcess, 1)
}

// useQPCTime controls whether time.now and nanotime use QueryPerformanceCounter.
// This is only set to 1 when running under Wine.
var useQPCTime uint8

var qpcStartCounter int64
var qpcMultiplier int64

//go:nosplit
func nanotimeQPC() int64 {
	var counter int64 = 0
	stdcall1(_QueryPerformanceCounter, uintptr(unsafe.Pointer(&counter)))

	// returns number of nanoseconds
	return (counter - qpcStartCounter) * qpcMultiplier
}

//go:nosplit
func nowQPC() (sec int64, nsec int32, mono int64) {
	var ft int64
	stdcall1(_GetSystemTimeAsFileTime, uintptr(unsafe.Pointer(&ft)))

	t := (ft - 116444736000000000) * 100

	sec = t / 1000000000
	nsec = int32(t - sec*1000000000)

	mono = nanotimeQPC()
	return
}

func initWine(k32 uintptr) {
	_GetSystemTimeAsFileTime = windowsFindfunc(k32, []byte("GetSystemTimeAsFileTime\000"))
	if _GetSystemTimeAsFileTime == nil {
		throw("could not find GetSystemTimeAsFileTime() syscall")
	}

	_QueryPerformanceCounter = windowsFindfunc(k32, []byte("QueryPerformanceCounter\000"))
	_QueryPerformanceFrequency = windowsFindfunc(k32, []byte("QueryPerformanceFrequency\000"))
	if _QueryPerformanceCounter == nil || _QueryPerformanceFrequency == nil {
		throw("could not find QPC syscalls")
	}

	// We can not simply fallback to GetSystemTimeAsFileTime() syscall, since its time is not monotonic,
	// instead we use QueryPerformanceCounter family of syscalls to implement monotonic timer
	// https://msdn.microsoft.com/en-us/library/windows/desktop/dn553408(v=vs.85).aspx

	var tmp int64
	stdcall1(_QueryPerformanceFrequency, uintptr(unsafe.Pointer(&tmp)))
	if tmp == 0 {
		throw("QueryPerformanceFrequency syscall returned zero, running on unsupported hardware")
	}

	// This should not overflow, it is a number of ticks of the performance counter per second,
	// its resolution is at most 10 per usecond (on Wine, even smaller on real hardware), so it will be at most 10 millions here,
	// panic if overflows.
	if tmp > (1<<31 - 1) {
		throw("QueryPerformanceFrequency overflow 32 bit divider, check nosplit discussion to proceed")
	}
	qpcFrequency := int32(tmp)
	stdcall1(_QueryPerformanceCounter, uintptr(unsafe.Pointer(&qpcStartCounter)))

	// Since we are supposed to run this time calls only on Wine, it does not lose precision,
	// since Wine's timer is kind of emulated at 10 Mhz, so it will be a nice round multiplier of 100
	// but for general purpose system (like 3.3 Mhz timer on i7) it will not be very precise.
	// We have to do it this way (or similar), since multiplying QPC counter by 100 millions overflows
	// int64 and resulted time will always be invalid.
	qpcMultiplier = int64(timediv(1000000000, qpcFrequency, nil))

	useQPCTime = 1
}

//go:nosplit
func getRandomData(r []byte) {
	n := 0
	if stdcall2(_ProcessPrng, uintptr(unsafe.Pointer(&r[0])), uintptr(len(r)))&0xff != 0 {
		n = len(r)
	}
	extendRandom(r, n)
}

func goenvs() {
	// strings is a pointer to environment variable pairs in the form:
	//     "envA=valA\x00envB=valB\x00\x00" (in UTF-16)
	// Two consecutive zero bytes end the list.
	strings := unsafe.Pointer(stdcall0(_GetEnvironmentStringsW))
	p := (*[1 << 24]uint16)(strings)[:]

	n := 0
	for from, i := 0, 0; true; i++ {
		if p[i] == 0 {
			// empty string marks the end
			if i == from {
				break
			}
			from = i + 1
			n++
		}
	}
	envs = make([]string, n)

	for i := range envs {
		envs[i] = gostringw(&p[0])
		for p[0] != 0 {
			p = p[1:]
		}
		p = p[1:] // skip nil byte
	}

	stdcall1(_FreeEnvironmentStringsW, uintptr(strings))

	// We call these all the way here, late in init, so that malloc works
	// for the callback functions these generate.
	var fn any = ctrlHandler
	ctrlHandlerPC := compileCallback(*efaceOf(&fn), true)
	stdcall2(_SetConsoleCtrlHandler, ctrlHandlerPC, 1)

	monitorSuspendResume()
}

// exiting is set to non-zero when the process is exiting.
var exiting uint32

//go:nosplit
func exit(code int32) {
	// Disallow thread suspension for preemption. Otherwise,
	// ExitProcess and SuspendThread can race: SuspendThread
	// queues a suspension request for this thread, ExitProcess
	// kills the suspending thread, and then this thread suspends.
	lock(&suspendLock)
	atomic.Store(&exiting, 1)
	stdcall1(_ExitProcess, uintptr(code))
}

// write1 must be nosplit because it's used as a last resort in
// functions like badmorestackg0. In such cases, we'll always take the
// ASCII path.
//
//go:nosplit
func write1(fd uintptr, buf unsafe.Pointer, n int32) int32 {
	const (
		_STD_OUTPUT_HANDLE = ^uintptr(10) // -11
		_STD_ERROR_HANDLE  = ^uintptr(11) // -12
	)
	var handle uintptr
	switch fd {
	case 1:
		handle = stdcall1(_GetStdHandle, _STD_OUTPUT_HANDLE)
	case 2:
		handle = stdcall1(_GetStdHandle, _STD_ERROR_HANDLE)
	default:
		// assume fd is real windows handle.
		handle = fd
	}
	isASCII := true
	b := (*[1 << 30]byte)(buf)[:n]
	for _, x := range b {
		if x >= 0x80 {
			isASCII = false
			break
		}
	}

	if !isASCII {
		var m uint32
		isConsole := stdcall2(_GetConsoleMode, handle, uintptr(unsafe.Pointer(&m))) != 0
		// If this is a console output, various non-unicode code pages can be in use.
		// Use the dedicated WriteConsole call to ensure unicode is printed correctly.
		if isConsole {
			return int32(writeConsole(handle, buf, n))
		}
	}
	var written uint32
	stdcall5(_WriteFile, handle, uintptr(buf), uintptr(n), uintptr(unsafe.Pointer(&written)), 0)
	return int32(written)
}

var (
	utf16ConsoleBack     [1000]uint16
	utf16ConsoleBackLock mutex
)

// writeConsole writes bufLen bytes from buf to the console File.
// It returns the number of bytes written.
func writeConsole(handle uintptr, buf unsafe.Pointer, bufLen int32) int {
	const surr2 = (surrogateMin + surrogateMax + 1) / 2

	// Do not use defer for unlock. May cause issues when printing a panic.
	lock(&utf16ConsoleBackLock)

	b := (*[1 << 30]byte)(buf)[:bufLen]
	s := *(*string)(unsafe.Pointer(&b))

	utf16tmp := utf16ConsoleBack[:]

	total := len(s)
	w := 0
	for _, r := range s {
		if w >= len(utf16tmp)-2 {
			writeConsoleUTF16(handle, utf16tmp[:w])
			w = 0
		}
		if r < 0x10000 {
			utf16tmp[w] = uint16(r)
			w++
		} else {
			r -= 0x10000
			utf16tmp[w] = surrogateMin + uint16(r>>10)&0x3ff
			utf16tmp[w+1] = surr2 + uint16(r)&0x3ff
			w += 2
		}
	}
	writeConsoleUTF16(handle, utf16tmp[:w])
	unlock(&utf16ConsoleBackLock)
	return total
}

// writeConsoleUTF16 is the dedicated windows calls that correctly prints
// to the console regardless of the current code page. Input is utf-16 code points.
// The handle must be a console handle.
func writeConsoleUTF16(handle uintptr, b []uint16) {
	l := uint32(len(b))
	if l == 0 {
		return
	}
	var written uint32
	stdcall5(_WriteConsoleW,
		handle,
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(l),
		uintptr(unsafe.Pointer(&written)),
		0,
	)
	return
}

//go:nosplit
func semasleep(ns int64) int32 {
	const (
		_WAIT_ABANDONED = 0x00000080
		_WAIT_OBJECT_0  = 0x00000000
		_WAIT_TIMEOUT   = 0x00000102
		_WAIT_FAILED    = 0xFFFFFFFF
	)

	var result uintptr
	if ns < 0 {
		result = stdcall2(_WaitForSingleObject, getg().m.waitsema, uintptr(_INFINITE))
	} else {
		start := nanotime()
		elapsed := int64(0)
		for {
			ms := int64(timediv(ns-elapsed, 1000000, nil))
			if ms == 0 {
				ms = 1
			}
			result = stdcall4(_WaitForMultipleObjects, 2,
				uintptr(unsafe.Pointer(&[2]uintptr{getg().m.waitsema, getg().m.resumesema})),
				0, uintptr(ms))
			if result != _WAIT_OBJECT_0+1 {
				// Not a suspend/resume event
				break
			}
			elapsed = nanotime() - start
			if elapsed >= ns {
				return -1
			}
		}
	}
	switch result {
	case _WAIT_OBJECT_0: // Signaled
		return 0

	case _WAIT_TIMEOUT:
		return -1

	case _WAIT_ABANDONED:
		systemstack(func() {
			throw("runtime.semasleep wait_abandoned")
		})

	case _WAIT_FAILED:
		systemstack(func() {
			print("runtime: waitforsingleobject wait_failed; errno=", getlasterror(), "\n")
			throw("runtime.semasleep wait_failed")
		})

	default:
		systemstack(func() {
			print("runtime: waitforsingleobject unexpected; result=", result, "\n")
			throw("runtime.semasleep unexpected")
		})
	}

	return -1 // unreachable
}

//go:nosplit
func semawakeup(mp *m) {
	if stdcall1(_SetEvent, mp.waitsema) == 0 {
		systemstack(func() {
			print("runtime: setevent failed; errno=", getlasterror(), "\n")
			throw("runtime.semawakeup")
		})
	}
}

//go:nosplit
func semacreate(mp *m) {
	if mp.waitsema != 0 {
		return
	}
	mp.waitsema = stdcall4(_CreateEventA, 0, 0, 0, 0)
	if mp.waitsema == 0 {
		systemstack(func() {
			print("runtime: createevent failed; errno=", getlasterror(), "\n")
			throw("runtime.semacreate")
		})
	}
	mp.resumesema = stdcall4(_CreateEventA, 0, 0, 0, 0)
	if mp.resumesema == 0 {
		systemstack(func() {
			print("runtime: createevent failed; errno=", getlasterror(), "\n")
			throw("runtime.semacreate")
		})
		stdcall1(_CloseHandle, mp.waitsema)
		mp.waitsema = 0
	}
}

// 在可能 m.p==nil 的情况下运行，因此不允许写屏障。
// 此函数由 newosproc0 调用，因此也需要在没有栈保护的情况下运行。
//
//go:nowritebarrierrec
//go:nosplit
func newosproc(mp *m) {
	// 我们传递 0 作为栈大小来使用此二进制文件的默认值。
	// 使用 stdcall6 调用 CreateThread 函数，参数如下：
	// 第一个参数是线程优先级，我们传入 0 使用默认值；
	// 第二个参数是栈大小，我们传入 0 使用默认值；
	// 第三个参数是线程的入口函数地址，我们传入 tstart_stdcall；
	// 第四个参数是线程参数，我们传入 mp 的地址；
	// 第五个和第六个参数保留，通常设为 0。
	thandle := stdcall6(_CreateThread, 0, 0,
		abi.FuncPCABI0(tstart_stdcall), uintptr(unsafe.Pointer(mp)),
		0, 0)

	if thandle == 0 {
		if atomic.Load(&exiting) != 0 {
			// 如果与 ExitProcess 并发调用，可能导致 CreateThread 失败。如果发生这种情况，只需冻结此线程，让进程退出。
			// 见问题 #18253。
			lock(&deadlock)
			lock(&deadlock)
		}
		print("runtime: 创建新的 OS 线程失败（已有 ", mcount(), " 个线程; errno=", getlasterror(), "）\n")
		throw("runtime.newosproc")
	}

	// 如果该线程退出, 关闭 thandle 以避免线程对象泄漏。
	stdcall1(_CloseHandle, thandle)
}

// Used by the C library build mode. On Linux this function would allocate a
// stack, but that's not necessary for Windows. No stack guards are present
// and the GC has not been initialized, so write barriers will fail.
//
//go:nowritebarrierrec
//go:nosplit
func newosproc0(mp *m, stk unsafe.Pointer) {
	// TODO: this is completely broken. The args passed to newosproc0 (in asm_amd64.s)
	// are stacksize and function, not *m and stack.
	// Check os_linux.go for an implementation that might actually work.
	throw("bad newosproc0")
}

func exitThread(wait *atomic.Uint32) {
	// We should never reach exitThread on Windows because we let
	// the OS clean up threads.
	throw("exitThread")
}

// 调用以初始化一个新的m (包括bootstrap m)。
// 在父线程上调用 (主线程在引导的情况下)，可以分配内存。
func mpreinit(mp *m) {
}

//go:nosplit
func sigsave(p *sigset) {
}

//go:nosplit
func msigrestore(sigmask sigset) {
}

//go:nosplit
//go:nowritebarrierrec
func clearSignalHandlers() {
}

//go:nosplit
func sigblock(exiting bool) {
}

// 被调用来初始化一个新的 M 线程（包括引导 M）,在新线程上调用，不能分配内存。
// 函数确保了新创建的 M 有正确的线程句柄、进程 ID、高分辨率定时器（如果可用），以及设置了正确的栈边界，这些都是初始化 M 和其 g0 必需的。
//
// 1.复制线程句柄：使用 _DuplicateHandle 函数复制当前线程的句柄到 thandle。
// 2.锁定 M 的 threadLock：防止多个线程同时修改 M 的状态。
// 3.设置 M 的线程句柄和进程 ID：将复制的线程句柄赋给 M，并获取当前线程的 ID。
// 4.配置 usleep 定时器：如果 M 没有高分辨率定时器并且系统支持，创建一个高分辨率定时器。
// 5.解锁 threadLock：完成对 M 的修改后解锁。
// 6.查询栈基址：使用 _VirtualQuery 函数从操作系统获取当前栈的基址信息。
// 7.设置栈边界：基于获取到的栈基址和大小，设置栈底地址和栈保护区域，确保栈使用的安全性。
// 8.检查栈和 SP：验证栈和栈指针（SP）的有效性，确保它们不会超出预设的边界。
func minit() {
	var thandle uintptr
	// 复制当前线程句柄到 thandle。
	if stdcall7(_DuplicateHandle, currentProcess, currentThread, currentProcess, uintptr(unsafe.Pointer(&thandle)), 0, 0, _DUPLICATE_SAME_ACCESS) == 0 {
		print("runtime.minit: duplicatehandle failed; errno=", getlasterror(), "\n")
		throw("runtime.minit: duplicatehandle failed")
	}

	// 获取当前 g 的 M。
	mp := getg().m
	// 锁定 M 的 threadLock。
	lock(&mp.threadLock)
	// 设置 M 的线程句柄。
	mp.thread = thandle
	// 设置 M 的进程 ID。
	mp.procid = uint64(stdcall0(_GetCurrentThreadId))

	// 配置 usleep 定时器，如果可能的话。
	if mp.highResTimer == 0 && haveHighResTimer {
		// 创建高分辨率定时器。
		mp.highResTimer = createHighResTimer()
		if mp.highResTimer == 0 {
			print("runtime: CreateWaitableTimerEx failed; errno=", getlasterror(), "\n")
			throw("CreateWaitableTimerEx when creating timer failed")
		}
	}

	// 解锁 M 的 threadLock。
	unlock(&mp.threadLock)

	// 从操作系统查询真实的栈基址。目前我们正在一个小的假设栈上运行。

	// 定义内存基本信息结构体。
	var mbi memoryBasicInformation
	res := stdcall3(_VirtualQuery, uintptr(unsafe.Pointer(&mbi)), uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
	if res == 0 {
		print("runtime: VirtualQuery failed; errno=", getlasterror(), "\n")
		throw("VirtualQuery for stack base failed")
	}
	// 系统在栈底部留下一个 8K PAGE_GUARD 区域（理论上 VirtualQuery 不应该包含这个，但它确实包含）。
	// 添加额外的 8K 用于调用没有栈检查的 C 函数和 lastcontinuehandler。无论如何我们都不应该接近这个界限。
	// 计算栈基址。
	base := mbi.allocationBase + 16<<10
	// 检查栈边界的有效性。
	g0 := getg()
	if base > g0.stack.hi || g0.stack.hi-base > 64<<20 {
		print("runtime: g0 stack [", hex(base), ",", hex(g0.stack.hi), ")\n")
		throw("bad g0 stack")
	}
	// 设置栈底地址。
	g0.stack.lo = base
	// 设置第一个栈保护区域。
	g0.stackguard0 = g0.stack.lo + stackGuard
	// 设置第二个栈保护区域。
	g0.stackguard1 = g0.stackguard0
	// 检查 SP 的有效性。
	stackcheck()
}

// Called from dropm to undo the effect of an minit.
//
//go:nosplit
func unminit() {
	mp := getg().m
	lock(&mp.threadLock)
	if mp.thread != 0 {
		stdcall1(_CloseHandle, mp.thread)
		mp.thread = 0
	}
	unlock(&mp.threadLock)
}

// Called from exitm, but not from drop, to undo the effect of thread-owned
// resources in minit, semacreate, or elsewhere. Do not take locks after calling this.
//
//go:nosplit
func mdestroy(mp *m) {
	if mp.highResTimer != 0 {
		stdcall1(_CloseHandle, mp.highResTimer)
		mp.highResTimer = 0
	}
	if mp.waitsema != 0 {
		stdcall1(_CloseHandle, mp.waitsema)
		mp.waitsema = 0
	}
	if mp.resumesema != 0 {
		stdcall1(_CloseHandle, mp.resumesema)
		mp.resumesema = 0
	}
}

// Calling stdcall on os stack.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrier
//go:nosplit
func stdcall(fn stdFunction) uintptr {
	gp := getg()
	mp := gp.m
	mp.libcall.fn = uintptr(unsafe.Pointer(fn))
	resetLibcall := false
	if mp.profilehz != 0 && mp.libcallsp == 0 {
		// leave pc/sp for cpu profiler
		mp.libcallg.set(gp)
		mp.libcallpc = getcallerpc()
		// sp must be the last, because once async cpu profiler finds
		// all three values to be non-zero, it will use them
		mp.libcallsp = getcallersp()
		resetLibcall = true // See comment in sys_darwin.go:libcCall
	}
	asmcgocall(asmstdcallAddr, unsafe.Pointer(&mp.libcall))
	if resetLibcall {
		mp.libcallsp = 0
	}
	return mp.libcall.r1
}

//go:nosplit
func stdcall0(fn stdFunction) uintptr {
	mp := getg().m
	mp.libcall.n = 0
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&fn))) // it's unused but must be non-nil, otherwise crashes
	return stdcall(fn)
}

// stdcall1 用于调用 stdcall 调用约定的 C 函数，但仅支持一个参数。
// fn 参数是一个指向 C 函数的指针，a0 是传递给该函数的唯一参数。
//
//go:nosplit
//go:cgo_unsafe_args
func stdcall1(fn stdFunction, a0 uintptr) uintptr {
	mp := getg().m                                           // 获取当前 goroutine 关联的执行上下文 m
	mp.libcall.n = 1                                         // 设置 libcall 的参数数量为 1
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0))) // 将参数 a0 的地址设置为 libcall 的参数
	return stdcall(fn)                                       // 调用 stdcall 函数并返回结果
}

//go:nosplit
//go:cgo_unsafe_args
func stdcall2(fn stdFunction, a0, a1 uintptr) uintptr {
	mp := getg().m
	mp.libcall.n = 2
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0)))
	return stdcall(fn)
}

//go:nosplit
//go:cgo_unsafe_args
func stdcall3(fn stdFunction, a0, a1, a2 uintptr) uintptr {
	mp := getg().m
	mp.libcall.n = 3
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0)))
	return stdcall(fn)
}

//go:nosplit
//go:cgo_unsafe_args
func stdcall4(fn stdFunction, a0, a1, a2, a3 uintptr) uintptr {
	mp := getg().m
	mp.libcall.n = 4
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0)))
	return stdcall(fn)
}

//go:nosplit
//go:cgo_unsafe_args
func stdcall5(fn stdFunction, a0, a1, a2, a3, a4 uintptr) uintptr {
	mp := getg().m
	mp.libcall.n = 5
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0)))
	return stdcall(fn)
}

//go:nosplit
//go:cgo_unsafe_args
func stdcall6(fn stdFunction, a0, a1, a2, a3, a4, a5 uintptr) uintptr {
	mp := getg().m
	mp.libcall.n = 6
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0)))
	return stdcall(fn)
}

//go:nosplit
//go:cgo_unsafe_args
func stdcall7(fn stdFunction, a0, a1, a2, a3, a4, a5, a6 uintptr) uintptr {
	mp := getg().m
	mp.libcall.n = 7
	mp.libcall.args = uintptr(noescape(unsafe.Pointer(&a0)))
	return stdcall(fn)
}

// These must run on the system stack only.
func usleep2(dt int32)
func switchtothread()

//go:nosplit
func osyield_no_g() {
	switchtothread()
}

//go:nosplit
func osyield() {
	systemstack(switchtothread)
}

//go:nosplit
func usleep_no_g(us uint32) {
	dt := -10 * int32(us) // relative sleep (negative), 100ns units
	usleep2(dt)
}

// 以微秒为单位的睡眠函数，实现功能为程序在指定的微秒时间内进入睡眠状态。
//
//go:nosplit 表示不进行栈分割，以便在系统栈上执行该函数。
func usleep(us uint32) {
	systemstack(func() { // 在系统栈上执行以下函数。

		// 计算相对睡眠时间（负数），单位为100纳秒。
		dt := -10 * int64(us)

		// 如果高分辨率定时器可用，并且为当前 M 分配了其句柄，请使用高分辨率定时器。
		// 否则，退回到低分辨率定时器，低分辨率定时器不需要句柄。
		if haveHighResTimer && getg().m.highResTimer != 0 {
			h := getg().m.highResTimer
			stdcall6(_SetWaitableTimer, h, uintptr(unsafe.Pointer(&dt)), 0, 0, 0, 0) // 设置高分辨率定时器。
			stdcall3(_NtWaitForSingleObject, h, 0, 0)                                // 等待定时器到期唤醒。
		} else {
			usleep2(int32(dt)) // 使用低分辨率定时器进行睡眠。
		}
	})
}

func ctrlHandler(_type uint32) uintptr {
	var s uint32

	switch _type {
	case _CTRL_C_EVENT, _CTRL_BREAK_EVENT:
		s = _SIGINT
	case _CTRL_CLOSE_EVENT, _CTRL_LOGOFF_EVENT, _CTRL_SHUTDOWN_EVENT:
		s = _SIGTERM
	default:
		return 0
	}

	if sigsend(s) {
		if s == _SIGTERM {
			// Windows terminates the process after this handler returns.
			// Block indefinitely to give signal handlers a chance to clean up,
			// but make sure to be properly parked first, so the rest of the
			// program can continue executing.
			block()
		}
		return 1
	}
	return 0
}

// called from zcallback_windows_*.s to sys_windows_*.s
func callbackasm1()

var profiletimer uintptr

func profilem(mp *m, thread uintptr) {
	// Align Context to 16 bytes.
	var c *context
	var cbuf [unsafe.Sizeof(*c) + 15]byte
	c = (*context)(unsafe.Pointer((uintptr(unsafe.Pointer(&cbuf[15]))) &^ 15))

	c.contextflags = _CONTEXT_CONTROL
	stdcall2(_GetThreadContext, thread, uintptr(unsafe.Pointer(c)))

	gp := gFromSP(mp, c.sp())

	sigprof(c.ip(), c.sp(), c.lr(), gp, mp)
}

func gFromSP(mp *m, sp uintptr) *g {
	if gp := mp.g0; gp != nil && gp.stack.lo < sp && sp < gp.stack.hi {
		return gp
	}
	if gp := mp.gsignal; gp != nil && gp.stack.lo < sp && sp < gp.stack.hi {
		return gp
	}
	if gp := mp.curg; gp != nil && gp.stack.lo < sp && sp < gp.stack.hi {
		return gp
	}
	return nil
}

func profileLoop() {
	stdcall2(_SetThreadPriority, currentThread, _THREAD_PRIORITY_HIGHEST)

	for {
		stdcall2(_WaitForSingleObject, profiletimer, _INFINITE)
		first := (*m)(atomic.Loadp(unsafe.Pointer(&allm)))
		for mp := first; mp != nil; mp = mp.alllink {
			if mp == getg().m {
				// Don't profile ourselves.
				continue
			}

			lock(&mp.threadLock)
			// Do not profile threads blocked on Notes,
			// this includes idle worker threads,
			// idle timer thread, idle heap scavenger, etc.
			if mp.thread == 0 || mp.profilehz == 0 || mp.blocked {
				unlock(&mp.threadLock)
				continue
			}
			// Acquire our own handle to the thread.
			var thread uintptr
			if stdcall7(_DuplicateHandle, currentProcess, mp.thread, currentProcess, uintptr(unsafe.Pointer(&thread)), 0, 0, _DUPLICATE_SAME_ACCESS) == 0 {
				print("runtime: duplicatehandle failed; errno=", getlasterror(), "\n")
				throw("duplicatehandle failed")
			}
			unlock(&mp.threadLock)

			// mp may exit between the DuplicateHandle
			// above and the SuspendThread. The handle
			// will remain valid, but SuspendThread may
			// fail.
			if int32(stdcall1(_SuspendThread, thread)) == -1 {
				// The thread no longer exists.
				stdcall1(_CloseHandle, thread)
				continue
			}
			if mp.profilehz != 0 && !mp.blocked {
				// Pass the thread handle in case mp
				// was in the process of shutting down.
				profilem(mp, thread)
			}
			stdcall1(_ResumeThread, thread)
			stdcall1(_CloseHandle, thread)
		}
	}
}

func setProcessCPUProfiler(hz int32) {
	if profiletimer == 0 {
		timer := stdcall3(_CreateWaitableTimerA, 0, 0, 0)
		atomic.Storeuintptr(&profiletimer, timer)
		newm(profileLoop, nil, -1)
	}
}

func setThreadCPUProfiler(hz int32) {
	ms := int32(0)
	due := ^int64(^uint64(1 << 63))
	if hz > 0 {
		ms = 1000 / hz
		if ms == 0 {
			ms = 1
		}
		due = int64(ms) * -10000
	}
	stdcall6(_SetWaitableTimer, profiletimer, uintptr(unsafe.Pointer(&due)), uintptr(ms), 0, 0, 0)
	atomic.Store((*uint32)(unsafe.Pointer(&getg().m.profilehz)), uint32(hz))
}

const preemptMSupported = true

// suspendLock protects simultaneous SuspendThread operations from
// suspending each other.
var suspendLock mutex

// 函数请求操作系统暂停并抢占指定的 M，以便实现 Goroutine 的抢占式调度。
func preemptM(mp *m) {
	// 防止自我抢占。
	if mp == getg().m {
		throw("self-preempt")
	}

	// 同步外部代码，可能尝试 ExitProcess。
	if !atomic.Cas(&mp.preemptExtLock, 0, 1) {
		// 外部代码正在运行。抢占尝试失败。
		mp.preemptGen.Add(1)
		return
	}

	lock(&mp.threadLock)

	// 获取对 mp 的线程的引用。
	if mp.thread == 0 {
		// M 尚未初始化或刚刚被反初始化。
		unlock(&mp.threadLock)
		atomic.Store(&mp.preemptExtLock, 0)
		mp.preemptGen.Add(1)
		return
	}

	// 创建线程的句柄以及进行线程上下文的准备
	// （这一部分包括挂起线程、获取线程上下文、注入异步抢占点）
	// 详细的实现细节涉及到不同的 CPU 架构处理，如 x86、amd64、arm 和 arm64。

	var thread uintptr
	if stdcall7(_DuplicateHandle, currentProcess, mp.thread, currentProcess, uintptr(unsafe.Pointer(&thread)), 0, 0, _DUPLICATE_SAME_ACCESS) == 0 {
		print("runtime.preemptM: duplicatehandle failed; errno=", getlasterror(), "\n")
		throw("runtime.preemptM: duplicatehandle failed")
	}

	unlock(&mp.threadLock)

	// 准备线程上下文缓冲区。必须对齐至 16 字节。
	var c *context
	var cbuf [unsafe.Sizeof(*c) + 15]byte
	c = (*context)(unsafe.Pointer((uintptr(unsafe.Pointer(&cbuf[15]))) &^ 15))
	c.contextflags = _CONTEXT_CONTROL

	lock(&suspendLock)

	// 序列化线程挂起。SuspendThread 是异步的，可能两个线程相互挂起并死锁。
	// 必须持有此锁直到 GetThreadContext 后，因为该函数会阻塞直到线程实际挂起。

	// 异步挂起线程。
	if int32(stdcall1(_SuspendThread, thread)) == -1 {
		unlock(&suspendLock)
		stdcall1(_CloseHandle, thread)
		atomic.Store(&mp.preemptExtLock, 0)
		// 线程不再存在。虽然不应该发生，但确认请求。
		mp.preemptGen.Add(1)
		return
	}

	// 我们必须非常小心，在这一点和显示 mp 处于异步安全点之间。
	// 类似信号处理器，mp 可以在我们停止它时做任何事情，包括持有任意锁。

	// 必须在检查 M 之前获取线程上下文，因为 SuspendThread 只请求挂起。
	// GetThreadContext 实际上会阻塞直到线程被挂起。
	// 1. 获取线程的寄存器值：可以获得线程在被挂起时各寄存器的当前值，如栈指针、指令指针等。
	// 2. 获取线程的运行状态：可以获知线程被挂起时处于的具体状态，有助于后续对线程的调度和操作。
	// 3. 为后续操作提供正确的上下文信息：获取线程上下文后，可以根据实际情况进行后续的处理，如根据上下文信息进行异步抢占点的注入等操作。
	stdcall2(_GetThreadContext, thread, uintptr(unsafe.Pointer(c)))

	unlock(&suspendLock)

	// 它是否想要抢占并且安全抢占？
	gp := gFromSP(mp, c.sp())
	if gp != nil && wantAsyncPreempt(gp) {
		if ok, newpc := isAsyncSafePoint(gp, c.ip(), c.sp(), c.lr()); ok {
			// 进行异步抢占点的注入
			// 注入调用 asyncPreempt
			targetPC := abi.FuncPCABI0(asyncPreempt)
			switch GOARCH {
			default:
				throw("unsupported architecture")
			case "386", "amd64":
				// 获取当前栈指针的位置，然后向栈指针减去 goarch.PtrSize，以便为注入的数据腾出空间
				sp := c.sp()
				sp -= goarch.PtrSize

				// 将新的目标 PC 程序计数器地址 newpc 写入到栈指针指向的位置，这样在执行返回指令时会跳转到异步抢占函数，实现抢占点的触发
				*(*uintptr)(unsafe.Pointer(sp)) = newpc

				// 更新上下文中的栈指针和指令指针为注入异步抢占点后的新值，以便后续的上下文设置能够正确执行。
				c.set_sp(sp)
				c.set_ip(targetPC)

			case "arm":
				// 获取当前栈指针的位置，然后向栈指针减去 goarch.PtrSize，以便为注入的数据腾出空间
				sp := c.sp()
				sp -= goarch.PtrSize

				// 更新线程上下文的栈指针为调整后的新值，确保后续操作在正确的栈位置进行
				c.set_sp(sp)
				// 将当前的 Link Register（LR）的值写入到栈指针指向的位置，保存当前的返回地址
				*(*uint32)(unsafe.Pointer(sp)) = uint32(c.lr())

				// LR 指向了异步抢占函数的地址，IP 指向了异步抢占点的目标地址，确保在恢复线程执行时能够正确跳转到异步抢占函数
				c.set_lr(newpc - 1)
				c.set_ip(targetPC)

			case "arm64":
				// 获取当前栈指针的位置，然后向栈指针减去 goarch.PtrSize，以便为注入的数据腾出空间
				sp := c.sp() - 16 // SP 需要 16 字节对齐

				// 更新线程上下文的栈指针为调整后的新值，确保后续操作在正确的栈位置进行
				c.set_sp(sp)
				// 将当前的 Link Register（LR）的值写入到栈指针指向的位置，保存当前的返回地址
				*(*uint64)(unsafe.Pointer(sp)) = uint64(c.lr())

				// LR 指向了异步抢占函数的地址，IP 指向了异步抢占点的目标地址，确保在恢复线程执行时能够正确跳转到异步抢占函数
				c.set_lr(newpc)
				c.set_ip(targetPC)
			}

			// 将修改后的上下文设置回线程，以实现 Goroutine 的暂停并转移
			stdcall2(_SetThreadContext, thread, uintptr(unsafe.Pointer(c)))
		}
	}

	// 清除 mp.preemptExtLock，表明抢占已完成。
	atomic.Store(&mp.preemptExtLock, 0)

	// 确认抢占。
	mp.preemptGen.Add(1)

	// 恢复线程并关闭句柄
	stdcall1(_ResumeThread, thread)
	stdcall1(_CloseHandle, thread)
}

// osPreemptExtEnter is called before entering external code that may
// call ExitProcess.
//
// This must be nosplit because it may be called from a syscall with
// untyped stack slots, so the stack must not be grown or scanned.
//
//go:nosplit
func osPreemptExtEnter(mp *m) {
	for !atomic.Cas(&mp.preemptExtLock, 0, 1) {
		// An asynchronous preemption is in progress. It's not
		// safe to enter external code because it may call
		// ExitProcess and deadlock with SuspendThread.
		// Ideally we would do the preemption ourselves, but
		// can't since there may be untyped syscall arguments
		// on the stack. Instead, just wait and encourage the
		// SuspendThread APC to run. The preemption should be
		// done shortly.
		osyield()
	}
	// Asynchronous preemption is now blocked.
}

// osPreemptExtExit is called after returning from external code that
// may call ExitProcess.
//
// See osPreemptExtEnter for why this is nosplit.
//
//go:nosplit
func osPreemptExtExit(mp *m) {
	atomic.Store(&mp.preemptExtLock, 0)
}
