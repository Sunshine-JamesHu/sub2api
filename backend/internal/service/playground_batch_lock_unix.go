//go:build !windows

package service

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockPlaygroundBatchStore(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = f.Close()
		return nil, errors.New("另一个服务实例正在管理批量上号任务")
	}
	return f, nil
}

func playgroundBatchDiskFree(path string) int64 {
	var state unix.Statfs_t
	if unix.Statfs(path, &state) != nil {
		return 0
	}
	return int64(state.Bavail) * int64(state.Bsize)
}
