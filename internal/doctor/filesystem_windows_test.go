// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"papio/internal/config"
	"papio/internal/pdf"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == pdf.WorkerArgument {
		switch os.Getenv("PAPIO_DOCTOR_TEST_WORKER") {
		case "invalid-json":
			fmt.Print("not JSON")
		case "wrong-response":
			fmt.Print("{}")
		case "multiple-responses":
			fmt.Print("{\"Reason\":\"open PDF failed\"} {}")
		case "oversized":
			fmt.Print(strings.Repeat("x", 2048))
		case "hang":
			time.Sleep(time.Minute)
		case "exit":
			os.Exit(1)
		default:
			if err := pdf.WorkerMain(os.Stdin, os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func windowsTestUser(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid
}

func windowsTestDescriptor(t *testing.T, sddl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse test security descriptor: %v", err)
	}
	return sd
}

func setWindowsTestDACL(t *testing.T, path, dacl string) {
	t.Helper()
	sd := windowsTestDescriptor(t, "D:P"+dacl)
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func makeTestConfigPublic(t *testing.T, path string) {
	t.Helper()
	setWindowsTestDACL(t, path, "(A;;FA;;;"+windowsTestUser(t).String()+")(A;;FR;;;WD)")
}

func TestWindowsPrivacyACLs(t *testing.T) {
	user := windowsTestUser(t)
	own := user.String()
	prefix := "O:" + own + "D:"
	private := "(A;;FA;;;" + own + ")(A;;FA;;;SY)(A;;FA;;;BA)"
	for _, tc := range []struct {
		name      string
		sddl      string
		directory bool
		wantOK    bool
	}{
		{"user only", prefix + "(A;;FA;;;" + own + ")", false, true},
		{"user system admins", prefix + private, false, true},
		{"observed config admins owner", "O:BAD:(A;ID;FA;;;SY)(A;ID;FA;;;BA)(A;ID;FA;;;" + own + ")", false, true},
		{"observed directory admins owner", "O:BAD:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;" + own + ")", true, true},
		{"empty DACL", prefix, false, true},
		{"null DACL", prefix + "NO_ACCESS_CONTROL", false, false},
		{"missing DACL", "O:" + own, false, false},
		{"other owner", "O:BUD:" + private, false, false},
		{"everyone read", prefix + private + "(A;;FR;;;WD)", false, false},
		{"users read", prefix + private + "(A;;FR;;;BU)", false, false},
		{"authenticated users write", prefix + private + "(A;;FW;;;AU)", false, false},
		{"unknown user read", prefix + private + "(A;;FR;;;S-1-5-21-1-2-3-1002)", false, false},
		{"everyone change DACL", prefix + private + "(A;;WD;;;WD)", false, false},
		{"deny is not grant", prefix + "(D;;FR;;;BU)" + private, false, true},
		{"attributes and ACL only", prefix + private + "(A;;0x00120080;;;WD)", false, true},
		{"directory traversal only", prefix + private + "(A;;FX;;;WD)", true, true},
		{"file execute", prefix + private + "(A;;FX;;;WD)", false, false},
		{"file irrelevant inherit only", prefix + private + "(A;OIIO;FR;;;WD)", false, true},
		{"directory inert inherit only", prefix + private + "(A;IO;FR;;;WD)", true, true},
		{"directory future files exposed", prefix + private + "(A;OIIO;FR;;;WD)", true, false},
		{"directory future folders exposed", prefix + private + "(A;CIIO;FR;;;WD)", true, false},
		{"creator owner inheritance", prefix + private + "(A;OICIIO;FA;;;CO)", true, true},
		{"owner rights", prefix + private + "(A;;FA;;;OW)", false, true},
		{"unsupported callback grant", prefix + private + "(XA;;FR;;;WD;(Member_of {SID(S-1-1-0)}))", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := windowsTestDescriptor(t, tc.sddl)
			err := checkWindowsPrivacy(sd, user, tc.directory)
			if (err == nil) != tc.wantOK {
				t.Fatalf("privacy = %v, want OK %v", err, tc.wantOK)
			}
		})
	}
	if err := checkWindowsPrivacy(nil, user, false); err == nil {
		t.Fatal("missing security descriptor passed")
	}
}

func TestWindowsDoctorUsesFileACLsAndLeavesThemUnchanged(t *testing.T) {
	own := windowsTestUser(t).String()
	for _, directory := range []bool{false, true} {
		for _, public := range []bool{false, true} {
			t.Run(fmt.Sprintf("directory=%v/public=%v", directory, public), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "target")
				if directory {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
					t.Fatal(err)
				}
				dacl := "(A;OICI;FA;;;" + own + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
				if public {
					dacl += "(A;;FR;;;WD)"
				}
				setWindowsTestDACL(t, path, dacl)
				before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
				if err != nil {
					t.Fatal(err)
				}
				cfg := config.Default()
				cfg.DataDir = ""
				cfg.Path = ""
				cfg.Browser.AdoptionRoot = filepath.Join(t.TempDir(), "papio")
				name := "config_permissions"
				if directory {
					cfg.DataDir = path
					name = "data_dir"
				} else {
					cfg.Path = path
				}
				// No database or worker is needed to observe these two checks.
				report := Run(context.Background(), cfg, nil, pdf.Capability{}, "", nil)
				var found bool
				for _, check := range report.Checks {
					if check.Name != name {
						continue
					}
					found = true
					if (check.Status == Pass) == public {
						t.Fatalf("public %v: %+v", public, check)
					}
					if public && (!strings.Contains(check.Remediation, "Properties > Security") || strings.Contains(check.Remediation, "chmod")) {
						t.Fatalf("non-Windows remediation: %+v", check)
					}
				}
				if !found {
					t.Fatalf("missing %s check", name)
				}
				after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
				if err != nil || before.String() != after.String() {
					t.Fatalf("doctor changed ACL: %v", err)
				}
			})
		}
	}
}

func TestWindowsDataDirChecksNewInheritedACL(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public=%v", public), func(t *testing.T) {
			parent := t.TempDir()
			dacl := "(A;OICI;FA;;;" + windowsTestUser(t).String() + ")"
			if public {
				dacl += "(A;OICI;FR;;;WD)"
			}
			setWindowsTestDACL(t, parent, dacl)
			path := filepath.Join(parent, "new-data")
			err := checkDataDir(path)
			if (err == nil) == public {
				t.Fatalf("new directory with public %v: %v", public, err)
			}
		})
	}
}

func TestWindowsPrivacyReadFailureDoesNotPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "removed")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if issue := checkPathPrivacy(path, info); issue == nil || !strings.Contains(issue.detail, "cannot be read") {
		t.Fatalf("unreadable descriptor passed: %+v", issue)
	}
}

func TestWindowsWorkerExecutableProbesRealProtocol(t *testing.T) {
	path := executable(t)
	if err := checkWorkerExecutable(context.Background(), path); err != nil {
		t.Fatalf("working worker rejected: %v", err)
	}
	for _, mode := range []string{"invalid-json", "wrong-response", "multiple-responses", "oversized", "exit", "hang"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PAPIO_DOCTOR_TEST_WORKER", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			if err := checkWorkerExecutable(ctx, path); err == nil {
				t.Fatal("broken worker passed")
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("worker probe did not honor deadline")
			}
		})
	}
	t.Run("default deadline", func(t *testing.T) {
		t.Setenv("PAPIO_DOCTOR_TEST_WORKER", "hang")
		// The caller's deadline is deliberately longer than the probe's own
		// five-second budget, so removing that budget makes this test fail.
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		start := time.Now()
		if err := checkWorkerExecutable(ctx, path); err == nil {
			t.Fatal("hung worker passed")
		}
		if time.Since(start) > 7*time.Second {
			t.Fatal("worker probe relied on the caller deadline")
		}
	})
	invalid := filepath.Join(t.TempDir(), "papio.exe")
	if err := os.WriteFile(invalid, []byte("not an executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{invalid, t.TempDir(), filepath.Join(t.TempDir(), "missing.exe")} {
		if err := checkWorkerExecutable(context.Background(), path); err == nil {
			t.Fatalf("non-executable %q passed", path)
		}
	}
	cfg := config.Default()
	cfg.DataDir, cfg.Path = "", ""
	cfg.Browser.AdoptionRoot = filepath.Join(t.TempDir(), "papio")
	report := Run(context.Background(), cfg, nil, pdf.Capability{}, invalid, nil)
	for _, check := range report.Checks {
		if check.Name == "pdf_worker" {
			if check.Status != Fail {
				t.Fatalf("invalid executable passed public health: %+v", check)
			}
			return
		}
	}
	t.Fatal("missing pdf_worker check")
}
