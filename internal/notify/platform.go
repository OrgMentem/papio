// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"os/exec"
	"runtime"
)

type platformMechanism uint8

const (
	platformUnavailable platformMechanism = iota
	platformMacOS
	platformLinuxNotifySend
	platformLinuxGDBus
	platformWindows
)

type platform struct {
	available bool
	detail    string
	mechanism platformMechanism
}

type linuxMechanism uint8

const (
	linuxNotifySend linuxMechanism = iota
	linuxGDBus
)

// PlatformCapability reports whether this build can invoke papio's local
// desktop notification channel. Delivery remains best effort because desktop
// notification services provide no acknowledgement that they displayed it.
func PlatformCapability() (available bool, detail string) {
	selected := probePlatform(runtime.GOOS, exec.LookPath)
	return selected.available, selected.detail
}

// NewPlatformSender constructs the sender selected for this host. A false
// result means the platform probe found no available notification mechanism.
func NewPlatformSender() (Sender, bool) {
	return newPlatformSender(runtime.GOOS, exec.LookPath, execCommand)
}

func newPlatformSender(goos string, lookPath func(string) (string, error), run ExecFunc) (Sender, bool) {
	selected := probePlatform(goos, lookPath)
	if !selected.available {
		return nil, false
	}
	switch selected.mechanism {
	case platformMacOS:
		return MacOS{Exec: run}, true
	case platformLinuxNotifySend:
		return Linux{Exec: run, mechanism: linuxNotifySend}, true
	case platformLinuxGDBus:
		return Linux{Exec: run, mechanism: linuxGDBus}, true
	case platformWindows:
		return Windows{Exec: run}, true
	default:
		return nil, false
	}
}

func probePlatform(goos string, lookPath func(string) (string, error)) platform {
	switch goos {
	case "darwin":
		if _, err := lookPath("osascript"); err != nil {
			return platform{detail: "desktop notifications are unavailable: macOS osascript is not installed"}
		}
		return platform{
			available: true,
			detail:    "desktop notifications available via macOS osascript (best effort; no delivery acknowledgement)",
			mechanism: platformMacOS,
		}
	case "linux":
		if _, err := lookPath("notify-send"); err == nil {
			return platform{
				available: true,
				detail:    "desktop notifications available via notify-send (best effort; no delivery acknowledgement)",
				mechanism: platformLinuxNotifySend,
			}
		}
		if _, err := lookPath("gdbus"); err == nil {
			return platform{
				available: true,
				detail:    "desktop notifications available via gdbus (best effort; no delivery acknowledgement)",
				mechanism: platformLinuxGDBus,
			}
		}
		return platform{detail: "desktop notifications are unavailable: neither notify-send nor gdbus is installed"}
	case "windows":
		if _, err := lookPath("powershell"); err != nil {
			return platform{detail: "desktop notifications are unavailable: powershell is not installed"}
		}
		return platform{
			available: true,
			detail:    "desktop notifications available via powershell WinRT toast (best effort; no delivery acknowledgement)",
			mechanism: platformWindows,
		}
	default:
		return platform{detail: "desktop notifications are unavailable on " + goos + " (supported platforms: macOS, Linux, and Windows)"}
	}
}
