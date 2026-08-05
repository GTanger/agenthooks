package agenthooks

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func parentPID() int { return os.Getppid() }

// procArgs reads another process's argv via the kern.procargs2 sysctl (same
// uid only, which is exactly the hook's situation). Layout: int32 argc,
// exec path, NUL padding, then argc NUL-separated argv strings.
func procArgs(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("agenthooks: short procargs2 for pid %d", pid)
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	if argc <= 0 {
		return nil, fmt.Errorf("agenthooks: invalid argc for pid %d", pid)
	}
	rest := raw[4:]
	// Skip the exec path and its NUL padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, fmt.Errorf("agenthooks: malformed procargs2 for pid %d", pid)
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	fields := bytes.Split(rest, []byte{0})
	args := make([]string, 0, argc)
	for _, f := range fields {
		if len(args) == argc {
			break
		}
		args = append(args, string(f))
	}
	if len(args) != argc {
		return nil, fmt.Errorf("agenthooks: incomplete procargs2 for pid %d", pid)
	}
	return args, nil
}

func procExecutable(pid int) (string, error) {
	const (
		procInfoCallPIDInfo = 2
		procPIDPathInfo     = 11
		maxPathLen          = 1024
	)
	buf := make([]byte, maxPathLen)
	ret, _, errno := syscall.Syscall6(
		syscall.SYS___PROC_INFO,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDPathInfo,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if errno != 0 {
		return "", errno
	}
	if ret == 0 {
		return "", fmt.Errorf("agenthooks: proc_pidpath failed for pid %d", pid)
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return string(buf), nil
}

// procPPID reads the parent pid from the process's kinfo_proc.
func procPPID(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Ppid), nil
}
