// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bootstrap

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"papio/internal/config"
	"papio/internal/protocol"
	"papio/internal/store/storetest"
)

func TestMissingNativeViewerHelperPreservesOrdinaryBootstrap(t *testing.T) {
	t.Setenv("PAPIO_TYPESAFE_API_KEY", "")
	cfg := config.Default()
	cfg.DataDir = storetest.DataDir(t)
	cfg.AccessMode = config.ModeDelegated
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	cfg.Browser.NativeViewerHelper = filepath.Join(t.TempDir(), "missing-helper")
	cfg.Browser.AdoptionRoot = filepath.Join(t.TempDir(), "Downloads", "papio")
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Error(err)
		}
	})
	hello, err := json.Marshal(map[string]any{
		"protocol": protocol.BrowserProtocolVersion, "type": "hello", "msg_id": "viewer-hello-001", "seq": 1,
		"payload": map[string]any{"extension_version": "1.2.3", "adapter_versions": map[string]string{},
			"features": []string{"native_viewer_download_v1", protocol.NativeViewerSaveFeature}},
	})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := system.Browser.Sync(context.Background(), "viewer-bootstrap-session", false, []json.RawMessage{hello})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range frames {
		frame, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != protocol.MsgHelloAck {
			continue
		}
		found = true
		if slices.Contains(frame.Payload.(*protocol.HelloAckPayload).Features, protocol.NativeViewerSaveFeature) {
			t.Fatal("missing helper advertised native save capability")
		}
	}
	if !found || system.App == nil || len(system.App.Resolvers) == 0 {
		t.Fatal("optional helper failure disabled ordinary acquisition")
	}
}
