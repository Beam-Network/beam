//go:build unix

package gpulock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const Supported = true

func Prepare(directory string) error {
	if err := os.MkdirAll(filepath.Dir(directory), 0o755); err != nil {
		return err
	}
	switch err := os.Mkdir(directory, 0o755); {
	case err == nil:
		return os.Chmod(directory, 0o777|os.ModeSticky)
	case !errors.Is(err, fs.ErrExist):
		return err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", directory)
	}
	if info.Mode().Perm()&0o002 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s is writable by every user without the sticky bit", directory)
	}
	return nil
}

func Exclusive(directory string, count, declared int64) ([]string, func(), error) {
	if err := Prepare(directory); err != nil {
		return nil, nil, err
	}
	var devices []string
	var locks []*os.File
	release := func() {
		for _, lock := range locks {
			_ = lock.Close()
		}
	}
	for device := int64(0); device < declared && int64(len(devices)) < count; device++ {
		lock, err := tryLock(directory, int(device), syscall.LOCK_EX)
		if err != nil {
			release()
			return nil, nil, err
		}
		if lock == nil {
			continue
		}
		locks = append(locks, lock)
		devices = append(devices, strconv.FormatInt(device, 10))
	}
	if int64(len(devices)) < count {
		release()
		return nil, nil, &BusyError{Free: int64(len(devices)), Requested: count}
	}
	return devices, release, nil
}

func Shared(directory string, device int) (*Lock, error) {
	if err := Prepare(directory); err != nil {
		return nil, err
	}
	file, err := tryLock(directory, device, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, &HeldError{Device: device}
	}
	return &Lock{file: file}, nil
}

func tryLock(directory string, device, how int) (*os.File, error) {
	lock, err := openLock(filepath.Join(directory, strconv.Itoa(device)+".lock"))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), how|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, nil
	}
	return lock, nil
}

func openLock(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	_ = lock.Chmod(0o644)
	return lock, nil
}
