// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build darwin || linux

package ownershipsnapshot

import (
	"os"
	"reflect"
	"syscall"
)

// Open nonblocking so a configured FIFO is rejected by Stat rather than
// turning a lookup into an unbounded wait.
func openSource(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, syscall.EBADF
	}
	return file, nil
}

func fileIdentity(_ *os.File, info os.FileInfo) fileID {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() != reflect.Pointer || stat.IsNil() {
		return fileID{}
	}
	stat = stat.Elem()
	device, ok := statUint(stat, "Dev")
	if !ok {
		return fileID{}
	}
	inode, ok := statUint(stat, "Ino")
	if !ok {
		return fileID{}
	}
	changed := stat.FieldByName("Ctimespec")
	if !changed.IsValid() {
		changed = stat.FieldByName("Ctim")
	}
	if !changed.IsValid() {
		return fileID{}
	}
	seconds, ok := statUint(changed, "Sec")
	if !ok {
		return fileID{}
	}
	nanoseconds, ok := statUint(changed, "Nsec")
	if !ok {
		return fileID{}
	}
	return fileID{
		device:  device,
		inode:   inode,
		changeA: seconds,
		changeB: nanoseconds,
		known:   true,
	}
}

func statUint(value reflect.Value, name string) (uint64, bool) {
	field := value.FieldByName(name)
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(field.Int()), true
	default:
		return 0, false
	}
}
