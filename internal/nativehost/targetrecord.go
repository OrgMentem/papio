// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package nativehost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

// The Windows host is a copy of papio.exe and the daemon target is a separate
// record file. Two files cannot be published atomically together, so the record
// names the digest of the host image it was written for. A reader accepts the
// pair only when the host image still hashes to that digest; an install that
// stops between the two publications leaves a mismatch the next host refuses,
// whichever file was published first.

const targetRecordDigestKey = "host-sha256="

// errHostPairMismatch reports a host image and daemon target record from
// different installs, or a record in an unrecognised format.
var errHostPairMismatch = errors.New("native-host image and daemon target are from different installs (run papio native-host install)")

// formatTargetRecord renders the daemon target record: the target path on the
// first line and the digest of the host image it belongs to on the second.
func formatTargetRecord(target, hostDigest string) string {
	return target + "\n" + targetRecordDigestKey + hostDigest + "\n"
}

// parseTargetRecord splits a record into its target path and host digest.
func parseTargetRecord(data []byte) (target, hostDigest string, err error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("%w: unrecognised target record", errHostPairMismatch)
	}
	target = strings.TrimSpace(lines[0])
	hostDigest, ok := strings.CutPrefix(strings.TrimSpace(lines[1]), targetRecordDigestKey)
	if target == "" || !ok || hostDigest == "" {
		return "", "", fmt.Errorf("%w: unrecognised target record", errHostPairMismatch)
	}
	return target, hostDigest, nil
}

// readFileShared reads path without blocking a concurrent rename or delete of it.
func readFileShared(path string) ([]byte, error) {
	file, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// digestFile returns the lowercase hex SHA-256 of the file at path. It opens
// the file with openShared so hashing never makes an installer's rename fail.
func digestFile(path string) (string, error) {
	file, err := openShared(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// verifyHostPair returns the daemon target from record only when hostDigest is
// the digest of the host image the record was written for. The target is
// returned even on a mismatch so status output can still name it.
func verifyHostPair(record []byte, hostDigest string) (string, error) {
	target, want, err := parseTargetRecord(record)
	if err != nil {
		return "", err
	}
	if hostDigest != want {
		return target, errHostPairMismatch
	}
	return target, nil
}

// pinnedHost is the digest of the image this process started from, or the
// error that stopped it being computed.
type pinnedHost struct {
	digest string
	err    error
}

var pinnedImage atomic.Pointer[pinnedHost]

// pinHostImage records the digest of the running host's own image once, at
// start. An install or rollback that swaps the file at the installed path
// afterwards then cannot make this process accept the new install's record.
func pinHostImage(path string) {
	digest, err := digestFile(path)
	pinnedImage.Store(&pinnedHost{digest: digest, err: err})
}

// runningHostDigest returns the pinned digest of the running host, falling back
// to hashing the installed image when no host pinned itself.
func runningHostDigest(installedPath string) (string, error) {
	if pinned := pinnedImage.Load(); pinned != nil {
		return pinned.digest, pinned.err
	}
	return digestFile(installedPath)
}
