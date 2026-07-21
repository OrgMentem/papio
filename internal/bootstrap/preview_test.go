// Copyright 2026 OrgMentem. Licensed under MIT.

package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"testing"
	"time"

	"papio/internal/config"
)

func TestSystemWiresAndClosesPreviewServer(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if system.Preview == nil {
		_ = system.Close()
		t.Fatal("bootstrap left preview server unwired")
	}

	pdf := []byte("%PDF-1.7\n%%EOF\n")
	path := cfg.DataDir + "/preview.pdf"
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		_ = system.Close()
		t.Fatal(err)
	}
	sum := sha256.Sum256(pdf)
	capabilityURL, err := system.Preview.Issue(1, path, hex.EncodeToString(sum[:]), int64(len(pdf)), time.Minute)
	if err != nil {
		_ = system.Close()
		t.Fatal(err)
	}
	response, err := http.Get(capabilityURL)
	if err != nil {
		_ = system.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = system.Close()
		t.Fatalf("preview status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if err := system.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(capabilityURL); err == nil {
		t.Fatal("preview URL remained reachable after system close")
	}
}
