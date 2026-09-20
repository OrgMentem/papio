// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"papio/internal/pdf"
)

func checkPathPrivacy(path string, info os.FileInfo) *privacyError {
	subject := "configuration"
	if info.IsDir() {
		subject = "data directory"
	}
	failure := func(err error) *privacyError {
		return &privacyError{
			detail: subject + " privacy could not be verified: " + err.Error(),
			remediation: "open Properties > Security > Advanced for " + path +
				"; restrict access to your Windows account, SYSTEM, and Administrators, including inherited permissions",
		}
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return failure(errors.New("current Windows user is unavailable"))
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return failure(errors.New("Windows security descriptor cannot be read"))
	}
	if err := checkWindowsPrivacy(sd, user.User.Sid, info.IsDir()); err != nil {
		return failure(err)
	}
	return nil
}

// checkWindowsPrivacy proves the ordinary per-user Windows ACL shape. SYSTEM
// and Administrators are trusted because they can already take ownership.
// Deny entries never grant access. We conservatively refuse unfamiliar ACL
// forms or outside grants even if a combination of deny entries might mask
// them; this is a privacy verification failure, not a claim of effective access.
func checkWindowsPrivacy(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID, directory bool) error {
	if sd == nil || !sd.IsValid() || user == nil || !user.IsValid() {
		return errors.New("Windows security descriptor is unavailable or invalid")
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && sid.IsValid() && (sid.Equals(user) ||
			sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner) {
		return errors.New("owner is outside the current user, SYSTEM, and Administrators")
	}
	dacl, _, err := sd.DACL()
	// A missing or NULL DACL grants everyone full access. An empty DACL does
	// the opposite; writability is checked separately by the data-dir probe.
	if err != nil || dacl == nil {
		return errors.New("Windows DACL is missing or unrestricted")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return errors.New("Windows DACL entry cannot be read")
		}
		inheritOnly := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0
		inheritable := ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != 0
		if inheritOnly && (!directory || !inheritable) {
			continue // This entry applies neither here nor to future data files.
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("Windows DACL contains an unsupported access entry")
		}
		// Reading ACLs/attributes or waiting on an object exposes no file
		// contents and permits no modification. Directory traversal alone is
		// similarly harmless, but an inherited execute grant applies to files.
		innocuous := windows.ACCESS_MASK(windows.READ_CONTROL | windows.SYNCHRONIZE | windows.FILE_READ_ATTRIBUTES)
		if directory && !inheritable {
			innocuous |= windows.FILE_EXECUTE
		}
		if ace.Mask & ^innocuous == 0 {
			continue
		}
		const sidOffset = unsafe.Offsetof(windows.ACCESS_ALLOWED_ACE{}.SidStart)
		if uintptr(ace.Header.AceSize) < sidOffset+8 {
			return errors.New("Windows DACL contains an invalid access entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || uintptr(sid.Len()) > uintptr(ace.Header.AceSize)-sidOffset {
			return errors.New("Windows DACL contains an invalid trustee")
		}
		if trusted(sid) || sid.IsWellKnown(windows.WinCreatorOwnerRightsSid) {
			continue
		}
		if directory && inheritOnly && sid.IsWellKnown(windows.WinCreatorOwnerSid) {
			continue // Replaced by the creating user's SID when inherited.
		}
		return errors.New("Windows DACL grants access beyond the current user, SYSTEM, and Administrators")
	}
	return nil
}

// Windows FileMode has no executable bits. Exercise the actual bounded worker
// protocol instead of either rejecting all .exe files or trusting the suffix.
// A fresh empty directory gives us a known-absent input without parsing a PDF.
func checkWorkerExecutable(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("papio worker executable is not a regular file")
	}
	probeDir, err := os.MkdirTemp("", "papio-doctor-worker-")
	if err != nil {
		return fmt.Errorf("create worker probe: %w", err)
	}
	defer os.Remove(probeDir)
	report, err := pdf.ValidateStructural(ctx, path, filepath.Join(probeDir, "absent.pdf"), pdf.StructuralOptions{
		Timeout: 5 * time.Second, MaxPages: 1, MaxOutputBytes: 1024,
	})
	if err != nil {
		return err
	}
	if report != (pdf.StructuralReport{Reason: "open PDF failed"}) {
		return errors.New("papio worker returned an unexpected probe response")
	}
	return nil
}
