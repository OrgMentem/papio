package browser

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The daemon gates several requests on features the EXTENSION advertises in its
// hello (`session.Features`, assigned from HelloPayload.Features). Requiring a
// feature the shipped extension never sends does not fail loudly anywhere: the
// frame parses, the hello is acked, the popup renders the button, and only the
// request is refused — as `extension_outdated`, which reads like stale software
// rather than a daemon bug. `pdf_grab_v1` shipped exactly that way, so "Send
// PDF" was refused in every browser, always.
//
// Go tests could not see it because they build their own hello
// (`grabCapableHello`, `helloWithFeatures`) containing whatever the case under
// test needs. That is the right shape for a unit test and the reason this file
// exists: nothing else compares the daemon's requirements against the wire
// behaviour the extension is actually pinned to.
//
// Both sides are read as string literals — the Go requirement from bridge.go's
// source, the extension's advertisement from the assertion in
// extension/test/background.test.ts that pins the emitted hello frame. No
// identifier resolution, and no third hand-maintained list to fall out of date.

// sessionFeatureValues maps every constant this package gates a SESSION's
// features on to its wire value. TestSessionGatedFeaturesAreAdvertised proves
// the map is exhaustive by parsing bridge.go, so omitting an entry fails rather
// than silently narrowing the check.
var sessionFeatureValues = map[string]string{
	"pdfGrabV1Feature":                        pdfGrabV1Feature,
	"effectPermitFeature":                     effectPermitFeature,
	"institutionalMaterializationFeature":     institutionalMaterializationFeature,
	"institutionalAuthenticationClaimFeature": institutionalAuthenticationClaimFeature,
}

var sessionFeatureGateRE = regexp.MustCompile(
	`slices\.Contains\((?:session|b\.holder|holder)\.Features,\s*(\w+)\)`)

var helloFeaturesAssertionRE = regexp.MustCompile(
	`(?s)payload\["features"\]\)\.toEqual\(\[(.*?)\]\)`)

var quotedRE = regexp.MustCompile(`"([a-z0-9_]+)"`)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// gatedSessionFeatures parses bridge.go for every feature gated on a session's
// advertised list, and returns their wire values.
func gatedSessionFeatures(t *testing.T) []string {
	t.Helper()
	src := repoFile(t, filepath.Join("internal", "browser", "bridge.go"))
	var values []string
	for _, match := range sessionFeatureGateRE.FindAllStringSubmatch(src, -1) {
		ident := match[1]
		value, ok := sessionFeatureValues[ident]
		if !ok {
			t.Fatalf("bridge.go gates session.Features on %s, which is missing "+
				"from sessionFeatureValues in this file — add it so this test "+
				"keeps checking every session-gated feature", ident)
		}
		if !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		t.Fatal("parsed no session feature gates from bridge.go; the regex has " +
			"drifted from the source and this test now proves nothing")
	}
	return values
}

// advertisedHelloFeatures returns the feature values the extension is pinned to
// send in its hello, read from the extension's own assertion.
func advertisedHelloFeatures(t *testing.T) []string {
	t.Helper()
	src := repoFile(t, filepath.Join("extension", "test", "background.test.ts"))
	match := helloFeaturesAssertionRE.FindStringSubmatch(src)
	if match == nil {
		t.Fatal("could not find the hello features assertion in " +
			"extension/test/background.test.ts; if that test was renamed or " +
			"relaxed, this cross-language check silently stops working")
	}
	var values []string
	for _, quoted := range quotedRE.FindAllStringSubmatch(match[1], -1) {
		values = append(values, quoted[1])
	}
	if len(values) == 0 {
		t.Fatalf("hello features assertion lists no features: %q", match[1])
	}
	return values
}

// TestSessionGatedFeaturesAreAdvertised fails when the daemon requires a
// session feature the shipped extension does not send. Such a requirement is
// unreachable in production while every Go test that supplies its own hello
// still passes.
func TestSessionGatedFeaturesAreAdvertised(t *testing.T) {
	advertised := advertisedHelloFeatures(t)
	for _, required := range gatedSessionFeatures(t) {
		if !slices.Contains(advertised, required) {
			t.Fatalf("bridge.go gates a request on session feature %q, but the "+
				"extension's hello advertises only [%s]. Either send %q from "+
				"the hello in extension/src/background.ts (and list it in the "+
				"assertion in extension/test/background.test.ts), or stop "+
				"gating on it — as written, that request is refused with "+
				"extension_outdated for every session in every browser.",
				required, strings.Join(advertised, ", "), required)
		}
	}
}
