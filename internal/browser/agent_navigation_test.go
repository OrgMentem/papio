// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"papio/internal/acquisitionagent"
	"papio/internal/protocol"
)

func TestAgentNavigationHelloNegotiatesPrerequisites(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for mask := 0; mask < 8; mask++ {
			t.Run(fmt.Sprintf("configured=%v/peer=%d", configured, mask), func(t *testing.T) {
				b, _, _, _ := newBridge(t)
				if configured {
					b.SetAcquisitionBackend(agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
						return acquisitionagent.Decision{}, nil
					}))
					t.Cleanup(b.CloseAcquisitionBackend)
				}
				peer := []string{nativeViewerDownloadFeature}
				for i, feature := range []string{agentFallbackFeature, protocol.NativeClickAdoptionFeature, protocol.AgentNavigationFeature} {
					if mask&(1<<i) != 0 {
						peer = append(peer, feature)
					}
				}
				raw, err := b.helloAck(sessionRoleHolder, SessionRolesMinExtensionVersion, peer)
				if err != nil {
					t.Fatal(err)
				}
				msg, err := protocol.DecodeBrowserMessage(raw)
				if err != nil {
					t.Fatal(err)
				}
				features := msg.Payload.(*protocol.HelloAckPayload).Features
				negotiated := configured && mask == 7
				if slices.Contains(features, protocol.AgentNavigationFeature) != negotiated {
					t.Fatalf("navigation=%v features=%v", negotiated, features)
				}
				if len(features) != len(b.Features) || len(features) > 32 || !slices.Contains(features, triageCountsSchema3Feature) || slices.Contains(features, triageCountsSchema2Feature) == negotiated {
					t.Fatalf("invalid feature packing: %v", features)
				}
				if !negotiated && mask == 0 {
					expected := slices.Clone(b.Features)
					expected[slices.Index(expected, triageSnapshotSchema2Feature)] = nativeViewerDownloadFeature
					if !slices.Equal(expected, features) {
						t.Fatalf("legacy features changed: %v", features)
					}
				}
			})
		}
	}
}
