// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"os"

	"golang.org/x/sys/windows"
)

func nativeSingleLink(f *os.File, _ os.FileInfo) bool {
	var info windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info) == nil && info.NumberOfLinks == 1 && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

// A directory cannot be flushed through a Windows handle; the copy's data is
// still synced before the link, as config.SyncDir's Windows variant does.
func syncNativeRootDir(*os.Root) error { return nil }
