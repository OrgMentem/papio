// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"strings"
)

// bodyChallengePrefixBytes bounds the response head inspected for anti-bot
// body markers. Challenge machinery (vendor scripts, interstitial titles,
// proof-of-work solvers) sits in the head of the document; the cap keeps the
// scan constant-cost no matter how large the response is. Measured (I11): every
// body-marker hit on 1096 stored bodies matches within the first 2 KiB, so
// 32 KiB leaves wide margin without reading the whole body.
const bodyChallengePrefixBytes = 32 * 1024

// challengeMarker pairs vendor structure (a script, widget, or solver token in
// the markup) with vendor text (a challenge title or message in the title or
// visible text). Both halves must match: a lone Turnstile widget fires on
// ordinary pages (measured: 127 doi.org 404 pages), and lone challenge text
// fires on articles about bot walls.
type challengeMarker struct {
	name  string
	match func(title, text, html string) bool
}

var bodyChallengeMarkers = []challengeMarker{
	{"fastly-client-challenge", func(title, text, html string) bool {
		return strings.Contains(strings.ToLower(html), "_fs-ch-") &&
			strings.Contains(strings.ToLower(title+"\n"+text), "challenge")
	}},
	{"aws-waf-gokuprops", func(title, text, html string) bool {
		lb := strings.ToLower(title + "\n" + text)
		return strings.Contains(strings.ToLower(html), "gokuprops") &&
			(strings.Contains(lb, "challenge") || strings.Contains(lb, "captcha") ||
				strings.Contains(lb, "bot"))
	}},
	{"aws-waf-text", func(title, text, html string) bool {
		lt := strings.ToLower(text)
		lh := strings.ToLower(html)
		// The structure half names a challenge form with a solver, never a
		// bare word: an article about bot walls carries "challenge" in its
		// own text (even in its title), so the word alone is not structure.
		hasSolverForm := strings.Contains(lh, "<form") &&
			(strings.Contains(lh, "captcha") || strings.Contains(lh, "gokuprops"))
		return strings.Contains(lt, "aws waf") &&
			(strings.Contains(lt, "challenge") || strings.Contains(lt, "captcha") ||
				strings.Contains(lt, "bot")) && hasSolverForm
	}},
	{"anubis", func(title, text, html string) bool {
		lh := strings.ToLower(html)
		lb := strings.ToLower(title + "\n" + text)
		return strings.Contains(lh, "anubis") &&
			(strings.Contains(lb, "bot") || strings.Contains(lb, "challenge") ||
				strings.Contains(lb, "proof-of-work") || strings.Contains(lb, "proof of work") ||
				strings.Contains(lb, "making sure"))
	}},
	{"proof-of-work", func(title, text, html string) bool {
		lh := strings.ToLower(html)
		lb := strings.ToLower(title + "\n" + text)
		hasSolver := strings.Contains(lh, "proof-of-work") || strings.Contains(lh, "proof of work") ||
			strings.Contains(lh, "solvepow") || strings.Contains(lh, "calculating solution")
		return hasSolver && (strings.Contains(lb, "security check") ||
			strings.Contains(lb, "verify your connection") || strings.Contains(lb, "please wait a moment"))
	}},
	{"pmc-recaptcha-browser-check", func(title, text, html string) bool {
		return strings.Contains(strings.ToLower(html), "recaptcha") &&
			strings.Contains(strings.ToLower(title+"\n"+text), "checking your browser")
	}},
	{"cloudflare-text-only", func(title, text, html string) bool {
		lb := strings.ToLower(title + "\n" + text)
		lh := strings.ToLower(html)
		return (strings.Contains(lb, "just a moment") ||
			strings.Contains(lb, "verifying you are human") ||
			strings.Contains(lb, "checking your browser")) &&
			(strings.Contains(lh, "cdn-cgi/challenge-platform") ||
				strings.Contains(lh, "__cf_chl") ||
				strings.Contains(lh, "cf-chl"))
	}},
}

// bodyChallenge reports the first anti-bot marker matching the bounded
// response prefix, or "" when there is none. A PDF prefix never matches: the
// guard keeps a 200 response that is a PDF from ever reading as a challenge.
// prefix must be the start of the body; pass at most bodyChallengePrefixBytes.
func bodyChallenge(prefix []byte) string {
	if len(prefix) == 0 || pdfLike(prefix) {
		return ""
	}
	html := string(prefix)
	title := challengeTitle(html)
	text := challengeText(html)
	for _, rule := range bodyChallengeMarkers {
		if rule.match(title, text, html) {
			return rule.name
		}
	}
	return ""
}

// challengeTitle extracts the document title, capped the way the scan needs.
func challengeTitle(html string) string {
	lower := strings.ToLower(html)
	i := strings.Index(lower, "<title")
	if i < 0 {
		return ""
	}
	j := strings.Index(lower[i:], ">")
	if j < 0 {
		return ""
	}
	k := strings.Index(lower[i+j:], "</title>")
	if k < 0 {
		return ""
	}
	title := html[i+j+1 : i+j+k]
	if len(title) > 256 {
		title = title[:256]
	}
	return strings.TrimSpace(title)
}

// challengeText strips tags and script bodies to visible text, capped. It
// matches the measured variant exactly, including its quirk: once a <script>
// opens, the in-script flag never clears, so text after the first script
// never counts. Challenge signals live in the title and the early text the
// scan keeps, which is what the measured recall rests on; "fixing" the flag
// changes what fires and needs a re-measurement.
func challengeText(html string) string {
	var sb strings.Builder
	inTag, inScript := false, false
	for i := 0; i < len(html); i++ {
		c := html[i]
		if c == '<' {
			tag := strings.ToLower(html[i:min(i+8, len(html))])
			if strings.HasPrefix(tag, "<script") {
				inScript = true
			}
			inTag = true
			continue
		}
		if c == '>' {
			inTag = false
			if inScript && i >= 8 && strings.ToLower(html[max(0, i-8):i+1]) == "/script>" {
				inScript = false
			}
			sb.WriteByte(' ')
			continue
		}
		if !inTag && !inScript {
			sb.WriteByte(c)
		}
	}
	text := sb.String()
	if len(text) > 40000 {
		text = text[:40000]
	}
	return text
}
