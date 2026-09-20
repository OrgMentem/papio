// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package config

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows mode bits do not restrict access. Seed a private parent, then prove
// that Save's newly created directory and atomic replacement inherit its ACL.
func configPermissionRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertSavedConfigPermissions(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{path, filepath.Dir(path)} {
		sd, err := windows.GetNamedSecurityInfo(item, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil || sd == nil {
			t.Fatalf("read saved config ACL: %v", err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 1 {
			t.Fatalf("saved config must inherit exactly one private grant: %s (%v)", sd.String(), err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE == 0 || !sid.Equals(user.User.Sid) ||
			ace.Mask&windows.FILE_GENERIC_READ != windows.FILE_GENERIC_READ ||
			ace.Mask&windows.FILE_GENERIC_WRITE != windows.FILE_GENERIC_WRITE {
			t.Fatalf("saved config did not inherit the current user's read/write grant: %s", sd.String())
		}
	}
}
