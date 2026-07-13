// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"reflect"
	"testing"

	"papio/internal/config"
)

func TestNewWiresResolverOrderAndCoreServices(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Errorf("close system: %v", err)
		}
	})
	var names []string
	for _, entry := range system.App.Resolvers {
		if entry.Adapter == nil {
			t.Fatal("nil resolver adapter")
		}
		names = append(names, entry.Adapter.Name())
	}
	want := []string{
		config.SourceArXiv,
		config.SourceEuropePMC,
		config.SourceUnpaywall,
		config.SourceOpenAlex,
		config.SourceCORE,
		config.SourceCrossrefTDM,
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("resolver order = %v, want %v", names, want)
	}
	if system.App.Fetch == nil || system.App.Validate == nil || system.Scheduler == nil || system.Bundle == nil {
		t.Fatal("bootstrap left a core service unwired")
	}
	if system.PDFCapability.PDFToPPM != "" || system.PDFCapability.Tesseract != "" {
		t.Fatal("OCR helpers remained enabled when pdf.ocr_enabled=false")
	}
}
