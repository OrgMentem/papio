// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/pdf"
)

type textCall struct {
	method string
	params map[string]any
}

func runArtifactsText(t *testing.T, result api.ArtifactTextResult, args ...string) (stdout, stderr string, call textCall, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, target any) error {
		raw, marshalErr := json.Marshal(params)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		call.method = method
		if decodeErr := json.Unmarshal(raw, &call.params); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		*target.(*api.ArtifactTextResult) = result
		return nil
	})
	root.SetArgs(append([]string{"artifacts", "text"}, args...))
	err = root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), call, err
}

func TestArtifactsTextJSONIsTheResultRecord(t *testing.T) {
	result := api.ArtifactTextResult{Text: "hello\n", FirstPage: 2, LastPage: 3, PagesTotal: 9, Chars: 6}
	stdout, _, call, err := runArtifactsText(t, result, "job_1", "--pages", "2-3", "--max-bytes", "4096", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if call.method != "artifacts.text_v1" || call.params["job_id"] != "job_1" ||
		call.params["first_page"] != float64(2) || call.params["last_page"] != float64(3) || call.params["max_bytes"] != float64(4096) {
		t.Fatalf("call = %+v", call)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("--json output is not one JSON object: %v (%q)", err, stdout)
	}
	for _, key := range []string{"text", "first_page", "last_page", "pages_total", "chars", "truncated", "ocr_used"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("--json output lacks %q: %v", key, decoded)
		}
	}
	if decoded["ocr_used"] != false || decoded["text"] != "hello\n" {
		t.Fatalf("decoded = %v", decoded)
	}
}

func TestArtifactsTextSHA256FlagAndSinglePage(t *testing.T) {
	_, _, call, err := runArtifactsText(t, api.ArtifactTextResult{}, strings.Repeat("a", 64), "--sha256", "--pages", "4")
	if err != nil {
		t.Fatal(err)
	}
	if call.params["sha256"] != strings.Repeat("a", 64) || call.params["job_id"] != nil ||
		call.params["first_page"] != float64(4) || call.params["last_page"] != float64(4) {
		t.Fatalf("call = %+v", call)
	}
}

func TestArtifactsTextRejectsMalformedPageRanges(t *testing.T) {
	for _, pages := range []string{"0", "3-2", "a-b", "-2", "2-", "1-2-3", "", "0-4"} {
		if _, _, _, err := runArtifactsText(t, api.ArtifactTextResult{}, "job_1", "--pages", pages); err == nil {
			t.Fatalf("--pages %q was accepted", pages)
		}
	}
	if _, _, _, err := runArtifactsText(t, api.ArtifactTextResult{}, "job_1", "--max-bytes", "0"); err == nil {
		t.Fatal("--max-bytes 0 was accepted")
	}
	if _, _, _, err := runArtifactsText(t, api.ArtifactTextResult{}, "job_1", "--max-bytes", "-5"); err == nil {
		t.Fatal("negative --max-bytes was accepted")
	}
}

func TestArtifactsTextPlainBranchStripsTerminalControls(t *testing.T) {
	hostile := "line one\n\x1b[31mred\x1b[0m \x07bell \u009b2J line\u0085two\x7f\npage two\x0c\x00end\n"
	stdout, _, _, err := runArtifactsText(t, api.ArtifactTextResult{Text: hostile, FirstPage: 1, LastPage: 2, PagesTotal: 2}, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range stdout {
		if r != '\n' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) {
			t.Fatalf("plain output carries control character %U: %q", r, stdout)
		}
	}
	if !strings.Contains(stdout, "line one\n") || !strings.Contains(stdout, "page two") || !strings.Contains(stdout, "end") {
		t.Fatalf("plain output lost its text or its line structure: %q", stdout)
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("plain output does not end in a newline: %q", stdout)
	}
}

func TestArtifactsTextTruncationNoteGoesToStderrNamingTheResumePage(t *testing.T) {
	stdout, stderr, _, err := runArtifactsText(t, api.ArtifactTextResult{Text: "abc", FirstPage: 3, LastPage: 5, PagesTotal: 20, Truncated: true}, "job_1", "--pages", "3-9")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "abc" {
		t.Fatalf("stdout = %q, want only the text", stdout)
	}
	if !strings.Contains(stderr, "truncated") || !strings.Contains(stderr, "--pages 5-") {
		t.Fatalf("stderr = %q, want a truncation note naming the resume page", stderr)
	}
}

func TestArtifactsTextHelpDocumentsTheByteCap(t *testing.T) {
	var out bytes.Buffer
	root := NewInProcessRoot(&out, &out, config.Config{}, nil)
	root.SetArgs([]string{"artifacts", "text", "--help"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "131072") && !strings.Contains(out.String(), "128 KiB") {
		t.Fatalf("help does not state the %d-byte cap:\n%s", pdf.MaxTextBytes, out.String())
	}
}
