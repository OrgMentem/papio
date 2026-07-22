// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package institution

import (
	"strings"
	"testing"
)

func TestDiscover(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Discovery
		wantErr string
	}{
		{
			name: "OpenURL resolver base retains institution settings",
			raw:  "  https://resolver.example.edu/openurl?url_ver=Z39.88-2004&rft.atitle=Example&institution=Example+University&vid=EXU:main&sid=google  ",
			want: Discovery{
				Kind:        KindOpenURL,
				OpenURLBase: "https://resolver.example.edu/openurl?institution=Example+University&vid=EXU%3Amain",
			},
		},
		{
			name: "Primo VE search URL",
			raw:  "https://university.primo.exlibrisgroup.com/discovery/search?vid=UNIV:Main&rft.title=An+Article&rft.au=Author&accountid=24680",
			want: Discovery{
				Kind:              KindPrimo,
				OpenURLBase:       "https://university.primo.exlibrisgroup.com/discovery/openurl?institution=UNIV&vid=UNIV%3AMain",
				ProquestAccountID: "24680",
			},
		},
		{
			name: "Primo without view identifier",
			raw:  "https://university.primo.exlibrisgroup.com/discovery/search?query=any,contains,history",
			want: Discovery{
				Kind: KindPrimo,
			},
		},
		{
			name: "SFX menu URL",
			raw:  "https://sfx.university.edu/sfx_local?sid=google&rft.atitle=Example&custid=university",
			want: Discovery{
				Kind:        KindSFX,
				OpenURLBase: "https://sfx.university.edu/sfx_local?custid=university",
			},
		},
		{
			name: "WorldCat Discovery URL",
			raw:  "https://university.on.worldcat.org/search/detail?queryString=history",
			want: Discovery{
				Kind:        KindWorldcat,
				OpenURLBase: "https://university.on.worldcat.org/atoztitles/link",
			},
		},
		{
			name: "EBSCO resolver",
			raw:  "https://resolver.ebscohost.com/openurl?custid=example&groupid=main&profile=eds&rft.title=Example",
			want: Discovery{
				Kind:        KindEBSCO,
				OpenURLBase: "https://resolver.ebscohost.com/openurl?custid=example&groupid=main&profile=eds",
			},
		},
		{
			name: "ProQuest OpenURL handler",
			raw:  "https://www.proquest.com/openurl?accountid=123456&rft.title=Example&institution=Example+University",
			want: Discovery{
				Kind:              KindProQuest,
				OpenURLBase:       "https://www.proquest.com/openurl?institution=Example+University",
				ProquestAccountID: "123456",
			},
		},
		{
			name: "Unknown discovery host",
			raw:  "https://discovery.example.edu/search?q=history",
			want: Discovery{
				Kind: KindUnknown,
			},
		},
		{
			name:    "HTTP is rejected",
			raw:     "http://resolver.example.edu/openurl",
			wantErr: "resolvers must be https",
		},
		{
			name:    "Garbage is rejected",
			raw:     "this is not a URL",
			wantErr: "absolute https URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Discover(test.raw)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Discover(%q) error = %v, want substring %q", test.raw, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover(%q) error = %v", test.raw, err)
			}
			if got.Kind != test.want.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, test.want.Kind)
			}
			if got.OpenURLBase != test.want.OpenURLBase {
				t.Errorf("OpenURLBase = %q, want %q", got.OpenURLBase, test.want.OpenURLBase)
			}
			if got.ProquestAccountID != test.want.ProquestAccountID {
				t.Errorf("ProquestAccountID = %q, want %q", got.ProquestAccountID, test.want.ProquestAccountID)
			}
			if got.Note == "" {
				t.Error("Note is empty")
			}
		})
	}
}
