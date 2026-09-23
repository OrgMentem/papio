// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"papio/internal/config"
)

var errNativeSource = errors.New("native download source rejected")

const nativeBaselineLimit = 4096

// Private memory only: the persistent reservation survives losing this
// baseline, but can never reconstruct or reopen it to authorize a retry.
type nativeDownloadRoot struct {
	source                               *os.Root
	landing                              *os.Root
	sourcePath, sourceAlias, landingPath string
	sourceInfo, landingInfo              os.FileInfo
	names                                map[string]bool
	identities                           []os.FileInfo
}

func (r *nativeDownloadRoot) close() {
	if r == nil {
		return
	}
	if r.source != nil {
		_ = r.source.Close()
	}
	if r.landing != nil {
		_ = r.landing.Close()
	}
}
func nativePathKey(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(s)
	}
	return s
}

func snapshotNativeDownloadRoot(ctx context.Context, cfg config.Config) (_ *nativeDownloadRoot, err error) {
	landing := filepath.Clean(cfg.EffectiveAdoptionRoot())
	if !filepath.IsAbs(landing) || !config.BrowserSteerableAdoptionRoot(landing) {
		return nil, errNativeSource
	}
	source := filepath.Dir(landing)
	home, _ := os.UserHomeDir()
	if source == filepath.Dir(source) || nativePathKey(source) == nativePathKey(filepath.Clean(home)) || nativePathKey(source) == nativePathKey(filepath.Clean(cfg.DataDir)) {
		return nil, errNativeSource
	}
	resolved, err := filepath.EvalSymlinks(source) // configured root only, never browser-supplied parents
	if err != nil {
		return nil, errNativeSource
	}
	if resolved == filepath.Dir(resolved) || nativePathKey(resolved) == nativePathKey(filepath.Clean(home)) || nativePathKey(resolved) == nativePathKey(filepath.Clean(cfg.DataDir)) {
		return nil, errNativeSource
	}
	r := &nativeDownloadRoot{sourcePath: resolved, sourceAlias: source, landingPath: filepath.Join(resolved, config.AdoptionDirName), names: map[string]bool{}}
	defer func() {
		if err != nil {
			r.close()
		}
	}()
	r.source, err = os.OpenRoot(resolved)
	if err != nil {
		return nil, errNativeSource
	}
	r.sourceInfo, err = r.source.Stat(".")
	if err != nil {
		return nil, errNativeSource
	}
	landingInfo, err := r.source.Lstat(config.AdoptionDirName)
	if err != nil || !landingInfo.IsDir() || landingInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errNativeSource
	}
	r.landing, err = r.source.OpenRoot(config.AdoptionDirName)
	if err != nil {
		return nil, errNativeSource
	}
	r.landingInfo, err = r.landing.Stat(".")
	if err != nil || !os.SameFile(landingInfo, r.landingInfo) {
		return nil, errNativeSource
	}
	dir, err := r.source.Open(".")
	if err != nil {
		return nil, errNativeSource
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(nativeBaselineLimit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errNativeSource
	}
	if len(entries) > nativeBaselineLimit {
		return nil, errNativeSource
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r.names[nativePathKey(e.Name())] = true
		info, eerr := r.source.Lstat(e.Name())
		if eerr != nil {
			return nil, errNativeSource
		}
		r.identities = append(r.identities, info)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return r, nil
}

func (r *nativeDownloadRoot) unchanged() bool {
	// Only configured paths are touched here; observed source parents never
	// become authority. Pinning plus identity checks reject root replacement.
	s, err := os.Stat(r.sourcePath)
	if err != nil || !os.SameFile(s, r.sourceInfo) {
		return false
	}
	l, err := r.source.Lstat(config.AdoptionDirName)
	return err == nil && l.Mode()&os.ModeSymlink == 0 && os.SameFile(l, r.landingInfo)
}

type nativeStagedFile struct {
	name, digest string
	size         int64
}

func (r *nativeDownloadRoot) stage(ctx context.Context, observed, reservation string, size, maxBytes int64) (_ nativeStagedFile, err error) {
	if !filepath.IsAbs(observed) || filepath.Clean(observed) != observed || size < 1 || size > maxBytes || !r.unchanged() {
		return nativeStagedFile{}, errNativeSource
	}
	parent := nativePathKey(filepath.Dir(observed))
	if parent != nativePathKey(r.sourcePath) && parent != nativePathKey(r.sourceAlias) {
		return nativeStagedFile{}, errNativeSource
	}
	name := filepath.Base(observed)
	if !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\:") || strings.TrimRight(name, ". ") != name || r.names[nativePathKey(name)] {
		return nativeStagedFile{}, errNativeSource
	}
	for _, suffix := range []string{".part", ".crdownload", ".download"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			return nativeStagedFile{}, errNativeSource
		}
	}
	if _, err := r.source.Lstat(name + ".part"); !errors.Is(err, os.ErrNotExist) {
		return nativeStagedFile{}, errNativeSource
	}
	before, err := r.source.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() != size {
		return nativeStagedFile{}, errNativeSource
	}
	for _, old := range r.identities {
		if os.SameFile(old, before) {
			return nativeStagedFile{}, errNativeSource
		}
	}
	in, err := r.source.Open(name)
	if err != nil {
		return nativeStagedFile{}, errNativeSource
	}
	defer func() { _ = in.Close() }()
	opened, err := in.Stat()
	if err != nil || !sameAdoptionFile(before, opened) || !nativeSingleLink(in, opened) {
		return nativeStagedFile{}, errNativeSource
	}
	if ctx.Err() != nil {
		return nativeStagedFile{}, ctx.Err()
	}
	stageName := "native_stage_" + reservation + ".tmp"
	out, err := r.landing.OpenFile(stageName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nativeStagedFile{}, errNativeSource
	}
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = r.landing.Remove(stageName)
		}
	}()
	h := sha256.New()
	buffer := make([]byte, 64*1024)
	var n int64
	for {
		if ctx.Err() != nil {
			return nativeStagedFile{}, ctx.Err()
		}
		read, rerr := in.Read(buffer)
		if read > 0 {
			n += int64(read)
			if n > size || n > maxBytes {
				return nativeStagedFile{}, errNativeSource
			}
			if _, werr := out.Write(buffer[:read]); werr != nil {
				return nativeStagedFile{}, errNativeSource
			}
			_, _ = h.Write(buffer[:read])
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nativeStagedFile{}, errNativeSource
		}
	}
	if err := out.Sync(); err != nil {
		return nativeStagedFile{}, errNativeSource
	}
	after, err := in.Stat()
	if err != nil || n != size || !sameAdoptionFile(before, after) || !nativeSingleLink(in, after) {
		return nativeStagedFile{}, errNativeSource
	}
	current, err := r.source.Lstat(name)
	if err != nil || !sameAdoptionFile(before, current) || !r.unchanged() {
		return nativeStagedFile{}, errNativeSource
	}
	if ctx.Err() != nil {
		return nativeStagedFile{}, ctx.Err()
	}
	if err := out.Close(); err != nil {
		return nativeStagedFile{}, errNativeSource
	}
	return nativeStagedFile{name: stageName, digest: hex.EncodeToString(h.Sum(nil)), size: n}, nil
}

func (r *nativeDownloadRoot) publish(ctx context.Context, jobID, filename string, staged nativeStagedFile) error {
	if !r.unchanged() || !filepath.IsLocal(jobID) || filepath.Base(jobID) != jobID {
		return errNativeSource
	}
	if err := r.landing.Mkdir(jobID, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errNativeSource
	}
	info, err := r.landing.Lstat(jobID)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errNativeSource
	}
	jobRoot, err := r.landing.OpenRoot(jobID)
	if err != nil {
		return errNativeSource
	}
	defer func() { _ = jobRoot.Close() }()
	pinned, err := jobRoot.Stat(".")
	if err != nil || !os.SameFile(info, pinned) {
		return errNativeSource
	}
	// A pinned job directory prevents an ancestor swap from redirecting
	// publication into another job. Partial copy blocks the ordinary sweep.
	part := filename + ".part"
	out, err := jobRoot.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errNativeSource
	}
	defer func() { _ = out.Close(); _ = jobRoot.Remove(part) }()
	in, err := r.landing.Open(staged.name)
	if err != nil {
		return errNativeSource
	}
	defer func() { _ = in.Close() }()
	h := sha256.New()
	buffer := make([]byte, 64*1024)
	var total int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > staged.size {
				return errNativeSource
			}
			if _, err := out.Write(buffer[:n]); err != nil {
				return errNativeSource
			}
			_, _ = h.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return errNativeSource
		}
	}
	if total != staged.size || hex.EncodeToString(h.Sum(nil)) != staged.digest {
		return errNativeSource
	}
	if err := out.Sync(); err != nil {
		return errNativeSource
	}
	if err := out.Close(); err != nil {
		return errNativeSource
	}
	current, err := r.landing.Lstat(jobID)
	if err != nil || !os.SameFile(info, current) || !r.unchanged() {
		return errNativeSource
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Link is exclusive; Rename would overwrite an existing name on Unix.
	if err := jobRoot.Link(part, filename); err != nil {
		return errNativeSource
	}
	return nil
}
