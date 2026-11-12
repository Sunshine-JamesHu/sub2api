//go:build windows

package service

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockPlaygroundBatchStore(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped) != nil {
		_ = f.Close()
		return nil, errors.New("另一个服务实例正在管理批量上号任务")
	}
	return f, nil
}

func playgroundBatchDiskFree(path string) int64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var available, total, free uint64
	if windows.GetDiskFreeSpaceEx(p, &available, &total, &free) != nil {
		return 0
	}
	return int64(available)
}
