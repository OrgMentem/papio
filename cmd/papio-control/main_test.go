// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"papio/internal/adaptercontrol"
)

var fixedNow = func() time.Time { return time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC) }

func runTool(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr, fixedNow)
	return stdout.String(), stderr.String(), err
}

func TestKeygenSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "control.pem")
	public, stderr, err := runTool(t, "keygen", "-out", keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "trust.go") {
		t.Fatalf("keygen did not say where the public key goes: %q", stderr)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private key mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	if _, _, err := runTool(t, "keygen", "-out", keyPath); err == nil {
		t.Fatal("keygen overwrote an existing private key")
	}
	public = strings.TrimSpace(public)
	key, err := adaptercontrol.ParsePublicKey(public)
	if err != nil {
		t.Fatalf("keygen printed %q: %v", public, err)
	}

	out := filepath.Join(dir, "adapter-control.json")
	if _, _, err := runTool(t, "sign", "-key", keyPath, "-sequence", "3", "-suspend-revision", "wiley@0.1.0",
		"-suspend-revision", "sciencedirect@0.3.0", "-suspend-domain", "publisher:www.sciencedirect.com", "-out", out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := adaptercontrol.Verify(raw, key)
	if err != nil {
		t.Fatalf("the signed file does not verify under the printed key: %v", err)
	}
	if verified.Document.Sequence != 3 || verified.Document.IssuedAt != "2026-10-12T09:30:00Z" ||
		strings.Join(verified.Document.SuspendedRevisions, ",") != "sciencedirect@0.3.0,wiley@0.1.0" {
		t.Fatalf("document = %+v (revisions must be sorted)", verified.Document)
	}
	stdout, _, err := runTool(t, "verify", "-pub", public, out)
	if err != nil || !strings.Contains(stdout, "sequence 3") {
		t.Fatalf("verify = %q, %v", stdout, err)
	}
	// A key written by keygen verifies nothing signed by another key.
	other := filepath.Join(dir, "other.pem")
	otherPublic, _, err := runTool(t, "keygen", "-out", other)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runTool(t, "verify", "-pub", strings.TrimSpace(otherPublic), out); err == nil {
		t.Fatal("verify accepted a document under the wrong public key")
	}
}

func TestSignAcceptsRawKeysAndADocumentFile(t *testing.T) {
	dir := t.TempDir()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	key := private.Public().(ed25519.PublicKey)
	for name, content := range map[string][]byte{
		"seed.bin":   seed,
		"full.bin":   private,
		"seed.b64":   []byte(base64.StdEncoding.EncodeToString(seed) + "\n"),
		"nonsense":   []byte("not a key"),
		"short.bin":  seed[:10],
		"empty.file": nil,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := filepath.Join(dir, "doc.json")
	if err := os.WriteFile(doc, []byte(`{"sequence":9,"key_id":"papio-control-1","suspended_revisions":[],"suspended_safety_domains":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seed.bin", "full.bin", "seed.b64"} {
		stdout, _, err := runTool(t, "sign", "-key", filepath.Join(dir, name), "-in", doc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		verified, err := adaptercontrol.Verify([]byte(stdout), key)
		if err != nil || verified.Document.Sequence != 9 || verified.Document.IssuedAt != "2026-10-12T09:30:00Z" {
			t.Fatalf("%s: %+v, %v", name, verified, err)
		}
	}
	for _, name := range []string{"nonsense", "short.bin", "empty.file"} {
		if _, _, err := runTool(t, "sign", "-key", filepath.Join(dir, name), "-in", doc); err == nil {
			t.Errorf("%s was accepted as a key", name)
		}
	}
}

func TestSignRefusesDocumentsTheDaemonWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k.pem")
	if _, _, err := runTool(t, "keygen", "-out", keyPath); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"sequence zero":      {"-sequence", "0"},
		"a host as revision": {"-sequence", "1", "-suspend-revision", "https://www.example.com@1.0.0"},
		"a domain with path": {"-sequence", "1", "-suspend-domain", "publisher:www.example.com/path"},
		"duplicates":         {"-sequence", "1", "-suspend-revision", "wiley@0.1.0", "-suspend-revision", "wiley@0.1.0"},
	} {
		if _, _, err := runTool(t, append([]string{"sign", "-key", keyPath}, args...)...); err == nil {
			t.Errorf("%s: the tool signed it", name)
		}
	}
	doc := filepath.Join(dir, "doc.json")
	_ = os.WriteFile(doc, []byte(`{"sequence":1,"key_id":"k","suspended_revisions":[],"suspended_safety_domains":[],"hosts":["x.example"]}`), 0o600)
	if _, _, err := runTool(t, "sign", "-key", keyPath, "-in", doc); err == nil {
		t.Error("a document with a host field was signed")
	}
	if _, _, err := runTool(t, "sign", "-key", keyPath, "-in", doc, "-sequence", "2"); err == nil {
		t.Error("-in combined with -sequence was accepted")
	}
}
