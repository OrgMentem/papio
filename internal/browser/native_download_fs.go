// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
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

// openNativeDownloadRoot pins the configured source and landing roots without
// listing either. Recovery uses it directly: it never scans user files.
func openNativeDownloadRoot(cfg config.Config) (_ *nativeDownloadRoot, err error) {
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
	return r, nil
}

func snapshotNativeDownloadRoot(ctx context.Context, cfg config.Config) (_ *nativeDownloadRoot, err error) {
	r, err := openNativeDownloadRoot(cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			r.close()
		}
	}()
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

// Recovery classification. Anything else (errNativeSource, a timeout, a busy
// gate, cancellation) is transient and never abandons admitted bytes.
var (
	errNativeStageMissing       = errors.New("native stage missing")
	errNativeStageRejected      = errors.New("native stage does not hold the admitted bytes")
	errNativePublicationBlocked = errors.New("native publication name is occupied")
)

// readNativeExact authenticates an open file against a prior Lstat and the
// admitted digest/size. Identity drift or different bytes return mismatch;
// only a read failure is transient.
func readNativeExact(ctx context.Context, f *os.File, before os.FileInfo, digest string, size int64, singleLink bool, mismatch error) error {
	opened, err := f.Stat()
	if err != nil {
		return errNativeSource
	}
	if !sameAdoptionFile(before, opened) || (singleLink && !nativeSingleLink(f, opened)) {
		return mismatch
	}
	h := sha256.New()
	buffer := make([]byte, 64*1024)
	var n int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		read, rerr := f.Read(buffer)
		if read > 0 {
			n += int64(read)
			if n > size {
				return mismatch
			}
			_, _ = h.Write(buffer[:read])
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return errNativeSource
		}
	}
	after, err := f.Stat()
	if err != nil {
		return errNativeSource
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest || !sameAdoptionFile(before, after) {
		return mismatch
	}
	return nil
}

// verifyStage re-authenticates retained daemon staging by its exact name. A
// link, a non-regular or hard-linked entry, or bytes other than the durable
// admission are rejected; nothing is chosen by recency or listing.
func (r *nativeDownloadRoot) verifyStage(ctx context.Context, name, digest string, size int64) (nativeStagedFile, error) {
	if !r.unchanged() {
		return nativeStagedFile{}, errNativeSource
	}
	_, err := verifyNativeExactIn(ctx, r.landing, name, digest, size, true, errNativeStageRejected)
	if errors.Is(err, os.ErrNotExist) {
		return nativeStagedFile{}, errNativeStageMissing
	}
	if err != nil {
		return nativeStagedFile{}, err
	}
	return nativeStagedFile{name: name, digest: digest, size: size}, nil
}

// verifyNativeExactIn authenticates one exact name, never following a link.
// A missing name returns os.ErrNotExist; a non-regular entry or different
// bytes return mismatch; only an I/O failure is transient.
func verifyNativeExactIn(ctx context.Context, root *os.Root, name, digest string, size int64, singleLink bool, mismatch error) (os.FileInfo, error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errNativeSource
	}
	if !before.Mode().IsRegular() || before.Size() != size {
		return nil, mismatch
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, errNativeSource
	}
	defer func() { _ = f.Close() }()
	if err := readNativeExact(ctx, f, before, digest, size, singleLink, mismatch); err != nil {
		return nil, err
	}
	return before, nil
}

// nativePrefixOf reports whether part is a regular single-link file whose
// bytes are a prefix of the already verified reference: the only shape a
// crash inside publish's copy can leave. Such a part holds nothing that the
// reference does not, so removing it cannot lose bytes.
func nativePrefixOf(ctx context.Context, partRoot *os.Root, part string, partInfo os.FileInfo, refRoot *os.Root, ref string, refInfo os.FileInfo) (bool, error) {
	if !partInfo.Mode().IsRegular() || partInfo.Size() > refInfo.Size() {
		return false, nil
	}
	pf, err := partRoot.Open(part)
	if err != nil {
		return false, errNativeSource
	}
	defer func() { _ = pf.Close() }()
	rf, err := refRoot.Open(ref)
	if err != nil {
		return false, errNativeSource
	}
	defer func() { _ = rf.Close() }()
	opened, perr := pf.Stat()
	refOpened, rerr := rf.Stat()
	if perr != nil || rerr != nil {
		return false, errNativeSource
	}
	if !sameAdoptionFile(partInfo, opened) || !nativeSingleLink(pf, opened) || !sameAdoptionFile(refInfo, refOpened) {
		return false, nil
	}
	got, want := make([]byte, 64*1024), make([]byte, 64*1024)
	var total int64
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		n, readErr := pf.Read(got)
		if n > 0 {
			total += int64(n)
			if total > refInfo.Size() {
				return false, nil
			}
			if _, err := io.ReadFull(rf, want[:n]); err != nil || !bytes.Equal(got[:n], want[:n]) {
				return false, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return false, errNativeSource
		}
	}
	after, err := pf.Stat()
	if err != nil {
		return false, errNativeSource
	}
	return total == partInfo.Size() && sameAdoptionFile(partInfo, after), nil
}

// resumePublication makes the job's final name hold exactly the admitted
// bytes, from an exact existing final copy (a crash after publish's link) or
// by publishing the verified stage. It never deletes the stage. A leftover
// part file is removed only when provably redundant: the same inode as the
// verified final copy, or a prefix of the admitted bytes (a crash mid-copy).
// Anything else at those names blocks publication and is kept.
func (r *nativeDownloadRoot) resumePublication(ctx context.Context, jobID, filename, stageName, digest string, size int64) error {
	if !r.unchanged() || !filepath.IsLocal(jobID) || filepath.Base(jobID) != jobID {
		return errNativeSource
	}
	var jobRoot *os.Root
	info, err := r.landing.Lstat(jobID)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return errNativeSource
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return errNativePublicationBlocked
	default:
		if jobRoot, err = r.landing.OpenRoot(jobID); err != nil {
			return errNativeSource
		}
		defer func() { _ = jobRoot.Close() }()
		if pinned, err := jobRoot.Stat("."); err != nil || !os.SameFile(info, pinned) {
			return errNativeSource
		}
	}
	var final os.FileInfo
	if jobRoot != nil {
		final, err = verifyNativeExactIn(ctx, jobRoot, filename, digest, size, false, errNativePublicationBlocked)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	refRoot, refName, refInfo := jobRoot, filename, final
	if final == nil {
		stage, err := verifyNativeExactIn(ctx, r.landing, stageName, digest, size, true, errNativeStageRejected)
		if errors.Is(err, os.ErrNotExist) {
			return errNativeStageMissing
		}
		if err != nil {
			return err
		}
		refRoot, refName, refInfo = r.landing, stageName, stage
	}
	if jobRoot != nil {
		part := filename + ".part"
		p, err := jobRoot.Lstat(part)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return errNativeSource
		default:
			redundant := final != nil && os.SameFile(p, final)
			if !redundant {
				if redundant, err = nativePrefixOf(ctx, jobRoot, part, p, refRoot, refName, refInfo); err != nil {
					return err
				}
			}
			if !redundant {
				return errNativePublicationBlocked
			}
			if err := jobRoot.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errNativeSource
			}
		}
	}
	if final != nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.publish(ctx, jobID, filename, nativeStagedFile{name: stageName, digest: digest, size: size})
}
