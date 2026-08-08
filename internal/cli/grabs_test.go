// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

func TestGrabIdentifierFlagsRequiresExactlyOne(t *testing.T) {
	for name, tc := range map[string]struct {
		doi, pmid, arxiv string
		wantErr          bool
	}{
		"none":     {wantErr: true},
		"multiple": {doi: "10.1000/test", pmid: "12345", wantErr: true},
		"doi":      {doi: "10.1000/test"},
		"pmid":     {pmid: "12345"},
		"arxiv":    {arxiv: "2401.00001"},
	} {
		t.Run(name, func(t *testing.T) {
			kind, value, err := grabIdentifierFlags(tc.doi, tc.pmid, tc.arxiv)
			if tc.wantErr {
				if err == nil {
					t.Fatal("grabIdentifierFlags succeeded; want error")
				}
				return
			}
			if err != nil || kind == "" || value == "" {
				t.Fatalf("grabIdentifierFlags = %q, %q, %v", kind, value, err)
			}
		})
	}
}
