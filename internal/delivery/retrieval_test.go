// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package delivery

import "testing"

func TestFulfillmentRetrievalURL(t *testing.T) {
	for _, test := range []struct {
		name              string
		baseURL           string
		providerReference string
		want              string
	}{
		{
			name:              "form-75 View PDF URL",
			baseURL:           "https://illiadweb.example.edu/illiad/illiad.dll",
			providerReference: "482910",
			want:              "https://illiadweb.example.edu/illiad/illiad.dll?Action=10&Form=75&Value=482910",
		},
		{
			name:              "provider reference is query-escaped",
			baseURL:           "https://illiadweb.example.edu/illiad/illiad.dll",
			providerReference: "TN 42/1",
			want:              "https://illiadweb.example.edu/illiad/illiad.dll?Action=10&Form=75&Value=TN+42%2F1",
		},
		{
			name:              "empty base_url yields no URL",
			baseURL:           "",
			providerReference: "482910",
			want:              "",
		},
		{
			name:              "empty provider reference yields no URL",
			baseURL:           "https://illiadweb.example.edu/illiad/illiad.dll",
			providerReference: "",
			want:              "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := FulfillmentRetrievalURL(test.baseURL, test.providerReference); got != test.want {
				t.Fatalf("FulfillmentRetrievalURL(%q, %q) = %q, want %q", test.baseURL, test.providerReference, got, test.want)
			}
		})
	}
}
