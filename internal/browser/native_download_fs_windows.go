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
