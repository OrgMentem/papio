// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package ownershipsnapshot

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openSource(path string) (*os.File, error) {
	return os.Open(path)
}

type fileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	fileAttributes uint32
}

func fileIdentity(file *os.File, _ os.FileInfo) fileID {
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &handleInfo); err != nil {
		return fileID{}
	}
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)),
		uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return fileID{}
	}
	return fileID{
		device:  uint64(handleInfo.VolumeSerialNumber),
		inode:   uint64(handleInfo.FileIndexHigh)<<32 | uint64(handleInfo.FileIndexLow),
		changeA: uint64(basic.changeTime),
		known:   true,
	}
}
