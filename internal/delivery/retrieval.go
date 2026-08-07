// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package delivery

import "net/url"

// ILLiad Web Platform query-string constants for the patron-web request
// page (2026-08-07 ADR-0017 amendment "Fulfillment retrieval"). ILLiad's
// classic web interface (illiad.dll) is driven entirely by numeric
// Action/Form query parameters rather than REST paths; these two are the
// only pair v1 ever constructs.
const (
	// illiadActionViewRequest is ILLiad's "view this specific transaction"
	// action (Action=10) — it opens the named Form against the Value
	// transaction number rather than a patron's whole request list.
	illiadActionViewRequest = "10"
	// illiadFormViewPDF is ILLiad's "View PDF" form (Form=75): the page
	// that serves (or links to) the document a lodged request was
	// fulfilled with. Documented here because ILLiad's own numbering is
	// otherwise opaque — there is no self-describing endpoint to derive it
	// from.
	illiadFormViewPDF = "75"
)

// FulfillmentRetrievalURL builds the patron-web "View PDF" URL for a
// fulfilled delivery request (2026-08-07 ADR-0017 amendment): baseURL is
// document_delivery.patron_web_base_url (distinct from BaseURL, the
// ILLiad Web Platform API base — never derived from it), and
// providerReference is the request's ILLiad TransactionNumber
// (delivery.Request.ProviderReference). The browser handoff drives this
// URL through the operator's own authenticated session; papio never
// authenticates to it directly. Returns "" when baseURL or
// providerReference is empty — callers must treat that as "cannot build a
// retrieval route", not as a URL to open.
func FulfillmentRetrievalURL(baseURL, providerReference string) string {
	if baseURL == "" || providerReference == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("Action", illiadActionViewRequest)
	q.Set("Form", illiadFormViewPDF)
	q.Set("Value", providerReference)
	u.RawQuery = q.Encode()
	return u.String()
}
