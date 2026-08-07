// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package bundle exports a ready job as a self-contained, schema-validated,
// provenance-digested AcquisitionBundle. Export is idempotent and never writes
// a live/signed candidate URL or credential.
package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"papio/internal/artifact"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/redact"
	"papio/internal/resolver"
)

// Exporter materializes bundles from the durable job/artifact stores.
type Exporter struct {
	Jobs      *job.Store
	Artifacts *artifact.Store
	DataDir   string
	Now       func() time.Time
}

// sourceRefRE bounds a resolver source name used as a cleartext entitlement
// reference. papio's licensed-API sources ("core", "crossref_tdm") are lowercase
// by construction; anything else omits the reference rather than emitting a
// value the consumer would refuse.
var sourceRefRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// acquisitionModeFor maps the accepted candidate onto the acquisition-bundle/2
// mode vocabulary. It is a derivation, not an inference: every input is a
// validated enum value papio already recorded on the candidate row.
//
// "manual" describes an artifact filed with no observed route at all, so it
// keeps no mode.
//
// "institutional" earns operator_browser_session only from recorded browser
// delivery context, never from the basis alone: a resolver-produced candidate
// carries this basis from its own paywall judgement, with no browser session
// behind it at all.
//
// Read the evidence values for what the extension actually means by them, not
// for what they sound like. currentSessionEvidence (extension/src/background.ts)
// tiers on the AGE of this origin's auth evidence, so "fresh_auth" means
// "positive evidence, newer than the TTL, that this origin was authenticated" —
// a live keepalive probe committing "in" produces it, and the extension reports
// that same observation to the daemon as "warm_verified". A witnessed login is
// only one of its producers, not its definition. "warm" is then the strictly
// weaker value: evidence present but aged past the TTL, so nobody has confirmed
// the session recently. That is why fresh_auth is the floor and warm is refused
// (ADR-0018 Decision 2) — recency of positive evidence, not the presence of a
// login event, is what separates them.
//
// So the mode claims exactly this: the bytes arrived through a browser session
// that was evidenced as authenticated at that origin. It does NOT claim the
// work was paywalled. The "oa" route cannot reach here — BrowserAccessBasis
// requires evidence "none" for it — but that closes only the case papio had
// ALREADY classified as open access before the handoff. An open-access file
// fetched through an institutional handoff still routes "direct" (the route is
// chosen from the daemon's pre-fetch requires_auth flag, not from the bytes),
// so it can carry this mode. That is the honest reading of the claim rather
// than a hole in it, but do not restate it as the hazard being unreachable.
//
// The gate is the recorded evidence, and the guard is one-way: an adoption with
// no recorded context stays entitlement-less, whether it predates migration
// 0019 or was adopted through a path that carried no context. ADR-0007's
// asymmetBrowserSessionFreshlyEvidencede invents rights evidence, a false
// negative costs nothing but a field.
func acquisitionModeFor(candidate *job.Candidate) (string, bool) {
	switch candidate.AccessBasis {
	case resolver.AccessOpen:
		return "open_access", true
	case resolver.AccessLicensedAPI:
		// papio sent its own configured API credential (CORE's bearer token,
		// Crossref's Plus token). That is a daemon-held credential by
		// definition, not future work.
		return "daemon_held_credential", true
	case resolver.AccessInstitutional:
		if !job.BrowserSessionFreshlyEvidenced(candidate.BrowserRoute, candidate.SessionEvidence) {
			return "", false
		}
		return "operator_browser_session", true
	default:
		return "", false
	}
}

// entitlementFor derives the v2 entitlement object, or nil when papio observed
// no route. Nil is the honest and common answer; the object is never partially
// filled.
func entitlementFor(candidate *job.Candidate) *protocol.BundleEntitlement {
	mode, ok := acquisitionModeFor(candidate)
	if !ok {
		return nil
	}
	route, ok := entitlementRoute(candidate)
	if !ok {
		return nil
	}
	entitlement := &protocol.BundleEntitlement{Route: route, AcquisitionMode: mode}
	// Only a daemon-held credential has an entitlement to name; open access
	// needs none.
	if mode == "daemon_held_credential" && sourceRefRE.MatchString(candidate.Source) {
		entitlement.EntitlementRef = "entitlement:source:" + candidate.Source
	}
	return entitlement
}

// entitlementRoute produces a bare origin from what papio actually fetched, or
// reports that none is available.
//
// redact.Host, never redact.URL: redact.URL deliberately appends "?<redacted>"
// when it strips a query, which is right for an operator log and is query data
// to a consumer. It also collapses an unparseable value to a placeholder rather
// than failing, so that placeholder is treated here as "no route" — papio omits
// the entitlement instead of shipping a string the consumer must reject.
func entitlementRoute(candidate *job.Candidate) (string, bool) {
	raw := candidate.URLRedacted
	if candidate.Source == "browser" && candidate.SessionEvidence != "" {
		// A browser adoption's candidate URL is the synthetic
		// "browser://adopted-download": the bytes arrived through the
		// operator's own browser rather than from a URL papio fetched, so that
		// value names no origin. The origin papio did observe is the page host
		// the extension reported at adoption, which
		// ApplyBrowserDeliveryContextToCandidate stored as landing_redacted.
		// This quotes what was recorded and never reconstructs an origin from
		// current config — the mutable-config hazard that kept this mode
		// producer-less.
		//
		// Both halves of the guard are load-bearing even though only one
		// writer can satisfy them today. session_evidence's sole writer is
		// gated to source='browser', so the pair is currently redundant; the
		// source check states the invariant actually relied on, because for any
		// other candidate URLRedacted IS the host papio fetched from (often a
		// CDN) and landing_redacted is a different page. A future writer of
		// session_evidence on a non-browser candidate would otherwise silently
		// repoint the route with no other symptom.
		raw = candidate.LandingRedacted
	} else if raw == "" {
		raw = candidate.LandingRedacted
	}
	if raw == "" {
		return "", false
	}
	host := redact.Host(raw)
	if !strings.HasPrefix(host, "https://") || strings.ContainsAny(host, "?#") {
		return "", false
	}
	return host, true
}

var exportDestinationLocks = struct {
	sync.Mutex
	locks map[string]*exportDestinationLock
}{locks: make(map[string]*exportDestinationLock)}

type exportDestinationLock struct {
	mu   sync.Mutex
	refs int
}

func lockExportDestination(destination string) func() {
	key, err := filepath.Abs(destination)
	if err != nil {
		key = destination
	}
	key = filepath.Clean(key)

	exportDestinationLocks.Lock()
	lock := exportDestinationLocks.locks[key]
	if lock == nil {
		lock = &exportDestinationLock{}
		exportDestinationLocks.locks[key] = lock
	}
	lock.refs++
	exportDestinationLocks.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		exportDestinationLocks.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(exportDestinationLocks.locks, key)
		}
		exportDestinationLocks.Unlock()
	}
}

// EncodeDocument renders a bundle exactly as bundle.json is written to disk, so
// a consumer reading bundle.document over IPC and a consumer reading the
// exported file see byte-identical documents.
func EncodeDocument(b *protocol.AcquisitionBundle) ([]byte, error) {
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Locate resolves the verified artifact for one ready job: the prerequisites a
// caller needs to open the bytes, and nothing about the bundle.
//
// Kept separate from Document deliberately. A ready job whose file hashes
// correctly can still fail bundle construction — no accepted candidate
// provenance, a per-acquisition identity that is not pass, or any other bundle
// field failing validation — and none of that makes the bytes harder to find.
// Routing artifacts.locate through Document would have made valid artifacts
// unlocatable for reasons that have nothing to do with locating them.
func (e *Exporter) Locate(ctx context.Context, jobID string) (*job.Artifact, error) {
	_, art, err := e.locate(ctx, jobID)
	return art, err
}

// locate also hands back the job row, so Document does not have to re-read it
// and build the bundle from a second, unguarded snapshot.
func (e *Exporter) locate(ctx context.Context, jobID string) (*job.Row, *job.Artifact, error) {
	if e.Jobs == nil || e.Artifacts == nil {
		return nil, nil, errors.New("bundle exporter missing stores")
	}
	row, err := e.Jobs.Get(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	if (row.State != job.StateReady && row.State != job.StateImported) || row.ArtifactSHA256 == "" {
		// A conflict, not an internal fault: "not collected yet" is the routine
		// answer a consumer polling for readiness gets, and mapping it to
		// `internal` both hides the reason and writes an error line per poll.
		return nil, nil, fmt.Errorf("%w: job %s is %s, not ready", job.ErrConflict, jobID, row.State)
	}
	art, err := e.Jobs.GetArtifact(ctx, row.ArtifactSHA256)
	if err != nil || art == nil {
		if err == nil {
			err = fmt.Errorf("job %s references missing artifact %s", jobID, row.ArtifactSHA256)
		}
		return nil, nil, err
	}
	// Verify before handing over a path: a caller must never be pointed at
	// bytes that no longer match the digest they are recorded under.
	if err := e.Artifacts.Verify(art.SHA256); err != nil {
		return nil, nil, err
	}
	// Return the path Verify actually hashed, not the stored column. Verify
	// resolves ArtifactPath(sha) against the store's CURRENT data directory,
	// while artifacts.path is first-writer-wins — UpsertArtifact's ON CONFLICT
	// updates identity_result and never path — so it stays pinned to whichever
	// data_dir first saw the digest. After a data_dir move the two diverge and
	// the column names bytes this call never checked.
	verified, err := e.Artifacts.ArtifactPath(art.SHA256)
	if err != nil {
		return nil, nil, err
	}
	art.Path = verified
	return row, art, nil
}

// Document builds and validates the bundle for one ready job and returns it
// without writing anything. bundle.export materialises; a ratified reader must
// not, so the two phases are separated here rather than at the handler.
func (e *Exporter) Document(ctx context.Context, jobID string) (*protocol.AcquisitionBundle, *job.Artifact, error) {
	row, art, err := e.locate(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	candidate, err := e.Jobs.CandidateForArtifact(ctx, jobID, art.SHA256)
	if err != nil {
		return nil, nil, err
	}
	if candidate == nil {
		return nil, nil, fmt.Errorf("artifact %s has no accepted candidate provenance", art.SHA256)
	}
	// Identity is a per-acquisition finding: artifacts.identity_result is shared
	// across every job holding the digest and is last-writer-wins, so a later
	// acquisition would otherwise rewrite this bundle's validation block
	// (ADR-0007).
	identity, err := e.Jobs.AcquisitionIdentity(ctx, jobID, art.SHA256)
	if err != nil {
		return nil, nil, err
	}
	if identity != "pass" && identity != "user_confirmed" {
		return nil, nil, fmt.Errorf("artifact identity is %q, not exportable", identity)
	}

	retrieved := art.CreatedAt
	if _, err := time.Parse(time.RFC3339, retrieved); err != nil {
		now := e.Now
		if now == nil {
			now = time.Now
		}
		retrieved = now().UTC().Format(time.RFC3339)
	}
	landing := candidate.LandingRedacted
	if strings.Contains(landing, "<redacted>") {
		landing = "" // do not export a syntactically fake or secret-bearing URI
	}
	b := &protocol.AcquisitionBundle{
		SchemaVersion: protocol.AcquisitionBundleSchemaVersionV2,
		JobID:         jobID, RequestID: row.WorkRequestID,
		Identity: protocol.BundleIdentity{
			DOI: row.Work.DOI, Title: row.Work.Title, Authors: append([]string(nil), row.Work.Authors...), Year: row.Work.Year,
		},
		Candidate: protocol.BundleCandidate{
			Source: candidate.Source, Version: candidate.Version, AccessBasis: candidate.AccessBasis,
			ReuseLicense: candidate.ReuseLicense, LandingURL: landing,
			Entitlement: entitlementFor(candidate),
		},
		RetrievedAt: retrieved,
		Artifact: protocol.BundleArtifact{
			SHA256: art.SHA256, SizeBytes: art.SizeBytes, MIME: art.MIME, PageCount: art.PageCount,
			TextChars: art.TextChars, OCRUsed: art.OCRUsed, Path: filepath.ToSlash(filepath.Join("artifacts", art.SHA256+".pdf")),
		},
		Validation:   protocol.BundleValidation{Structural: "pass", Identity: identity},
		ZotioItemKey: row.ZotioItemKey,
	}
	b.ProvenanceDigest, err = digest(b)
	if err != nil {
		return nil, nil, err
	}
	if err := b.Validate(); err != nil {
		return nil, nil, fmt.Errorf("bundle validation: %w", err)
	}
	return b, art, nil
}

// Export creates (or verifies and reuses) bundle.json and its relative
// content-addressed artifact. An empty destination uses DataDir/bundles/<job>.
func (e *Exporter) Export(ctx context.Context, jobID, destination string) (string, *protocol.AcquisitionBundle, error) {
	b, art, err := e.Document(ctx, jobID)
	if err != nil {
		return "", nil, err
	}
	if destination == "" {
		destination = filepath.Join(e.DataDir, "bundles", jobID)
	}
	unlock := lockExportDestination(destination)
	defer unlock()

	destinationExisted, err := pathExists(destination)
	if err != nil {
		return "", nil, err
	}
	artifactsDir := filepath.Join(destination, "artifacts")
	artifactsDirExisted, err := pathExists(artifactsDir)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(artifactsDir, 0o700); err != nil {
		return "", nil, err
	}
	artifactPath := filepath.Join(destination, filepath.FromSlash(b.Artifact.Path))
	artifactCreated, err := materializeArtifact(art.Path, artifactPath, art.SHA256)
	if err != nil {
		return "", nil, err
	}
	bundlePath := filepath.Join(destination, "bundle.json")
	rollback := func(primary error, bundleCreated bool) (string, *protocol.AcquisitionBundle, error) {
		cleanupErr := cleanupExport(destination, artifactsDir, artifactPath, bundlePath,
			destinationExisted, artifactsDirExisted, artifactCreated, bundleCreated)
		if cleanupErr != nil {
			return "", nil, fmt.Errorf("exporting bundle: %w", errors.Join(primary, cleanupErr))
		}
		return "", nil, primary
	}
	bundleExisted, err := pathExists(bundlePath)
	if err != nil {
		return rollback(err, false)
	}
	encoded, err := EncodeDocument(b)
	if err != nil {
		return rollback(err, false)
	}
	if err := atomicWrite(bundlePath, encoded, 0o600); err != nil {
		return rollback(err, false)
	}
	if err := e.record(ctx, jobID, art.SHA256, bundlePath); err != nil {
		return rollback(err, !bundleExisted)
	}
	return bundlePath, b, nil
}

func digest(b *protocol.AcquisitionBundle) (string, error) {
	copy := *b
	copy.ProvenanceDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func materializeArtifact(source, target, expectedSHA string) (created bool, retErr error) {
	if got, _, err := artifact.HashFile(target); err == nil {
		if got == expectedSHA {
			return false, nil
		}
		return false, fmt.Errorf("existing bundle artifact %s has hash %s, want %s", target, got, expectedSHA)
	} else if !os.IsNotExist(err) {
		return false, err
	}
	in, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := in.Close(); retErr == nil && closeErr != nil {
			retErr = closeErr
		}
	}()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return false, err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		if copyErr != nil {
			return false, copyErr
		}
		return false, closeErr
	}
	got, _, err := artifact.HashFile(target)
	if err != nil {
		_ = os.Remove(target)
		return false, err
	}
	if got != expectedSHA {
		_ = os.Remove(target)
		return false, fmt.Errorf("copied artifact hash %s, want %s", got, expectedSHA)
	}
	return true, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func cleanupExport(destination, artifactsDir, artifactPath, bundlePath string, destinationExisted, artifactsDirExisted, artifactCreated, bundleCreated bool) error {
	var errs []error
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if bundleCreated {
		remove(bundlePath)
	}
	if artifactCreated {
		remove(artifactPath)
	}
	if !artifactsDirExisted {
		remove(artifactsDir)
	}
	if !destinationExisted {
		remove(destination)
	}
	return errors.Join(errs...)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bundle-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (e *Exporter) record(ctx context.Context, jobID, sha, path string) error {
	key := "bundle:" + jobID + ":" + sha
	result, _ := json.Marshal(map[string]string{"artifact_sha256": sha})
	_, err := e.Jobs.S.DB().ExecContext(ctx, `
		INSERT INTO exports(job_id, kind, idempotency_key, path, result_json, created_at)
		VALUES(?, 'bundle', ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO UPDATE SET path = excluded.path, result_json = excluded.result_json`,
		jobID, key, path, string(result), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
