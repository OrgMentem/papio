//go:build !windows

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExtractPageTextPassesPageRangeAndReturnsOnlyThatText(t *testing.T) {
	path := writeTempPDF(t)
	// The fake echoes its arguments, so the test sees exactly what pdftotext
	// would have been told.
	tool := fakeTool(t, `printf 'ARGS:%s|' "$*"; printf 'page two\fpage three\f'`)
	got, err := ExtractPageText(context.Background(), path, Capability{PDFToText: tool}, SemanticOptions{},
		PageTextRequest{FirstPage: 2, LastPage: 3, PagesTotal: 9})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Text, "ARGS:-f 2 -l 3 "+path+" -|") {
		t.Fatalf("pdftotext arguments = %q, want -f 2 -l 3 <path> -", got.Text)
	}
	if got.FirstPage != 2 || got.LastPage != 3 || got.PagesTotal != 9 || got.Truncated {
		t.Fatalf("page text = %+v", got)
	}
	if !strings.Contains(got.Text, "page two") || !strings.Contains(got.Text, "page three") {
		t.Fatalf("text = %q", got.Text)
	}
}

func TestExtractPageTextClampsAndRejectsRanges(t *testing.T) {
	path := writeTempPDF(t)
	tool := fakeTool(t, `printf 'ARGS:%s' "$*"`)
	cap := Capability{PDFToText: tool}
	got, err := ExtractPageText(context.Background(), path, cap, SemanticOptions{},
		PageTextRequest{FirstPage: 2, LastPage: 99, PagesTotal: 4})
	if err != nil || got.LastPage != 4 || !strings.HasPrefix(got.Text, "ARGS:-f 2 -l 4 ") {
		t.Fatalf("clamp: %+v err=%v", got, err)
	}
	for _, req := range []PageTextRequest{
		{FirstPage: 5, LastPage: 5, PagesTotal: 4},
		{FirstPage: 3, LastPage: 2, PagesTotal: 4},
		{FirstPage: -1},
	} {
		if _, err := ExtractPageText(context.Background(), path, cap, SemanticOptions{}, req); !errors.Is(err, ErrInvalidPageRange) {
			t.Fatalf("req %+v err = %v, want ErrInvalidPageRange", req, err)
		}
	}
}

func TestExtractPageTextOverTheCapIsTruncatedAtAPageBoundaryReport(t *testing.T) {
	path := writeTempPDF(t)
	// Three pages of 40 bytes each; a 100-byte cap cuts inside page three.
	tool := fakeTool(t, `i=0; while [ "$i" -lt 3 ]; do printf '%039d\f' 0; i=$((i+1)); done`)
	got, err := ExtractPageText(context.Background(), path, Capability{PDFToText: tool}, SemanticOptions{},
		PageTextRequest{MaxBytes: 100, PagesTotal: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Text) != 100 {
		t.Fatalf("truncated=%v len=%d, want true/100", got.Truncated, len(got.Text))
	}
	if got.FirstPage != 1 || got.LastPage != 3 {
		t.Fatalf("pages %d-%d, want 1-3 (the cut falls inside page 3)", got.FirstPage, got.LastPage)
	}
	if got.Chars != 100 {
		t.Fatalf("chars = %d", got.Chars)
	}
}

func TestExtractPageTextNeverExceedsTheDocumentedCap(t *testing.T) {
	path := writeTempPDF(t)
	tool := fakeTool(t, `i=0; while [ "$i" -lt 40000 ]; do printf 'aaaaaaaaaaaaaaaa'; i=$((i+1)); done`)
	got, err := ExtractPageText(context.Background(), path, Capability{PDFToText: tool}, SemanticOptions{},
		PageTextRequest{MaxBytes: 10 * MaxTextBytes})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Text) != MaxTextBytes {
		t.Fatalf("truncated=%v len=%d, want true/%d", got.Truncated, len(got.Text), MaxTextBytes)
	}
}

// The cap is sized for JSON's worst case: every byte of untrusted extracted
// text escaping to six bytes must still leave room inside the IPC result cap.
func TestMaxTextBytesSurvivesWorstCaseJSONEscaping(t *testing.T) {
	worst := strings.Repeat("\x00", MaxTextBytes)
	encoded, err := json.Marshal(struct {
		Text string `json:"text"`
	}{worst})
	if err != nil {
		t.Fatal(err)
	}
	const ipcMaxResultBytes = 1 << 20 // internal/ipc.MaxResultBytes; pdf cannot import it
	if len(encoded)+4<<10 >= ipcMaxResultBytes {
		t.Fatalf("worst-case encoded text = %d bytes, leaves no envelope headroom under %d", len(encoded), ipcMaxResultBytes)
	}
}

func TestExtractPageTextTruncationKeepsValidUTF8(t *testing.T) {
	path := writeTempPDF(t)
	tool := fakeTool(t, `printf 'aé€€€€'`)
	got, err := ExtractPageText(context.Background(), path, Capability{PDFToText: tool}, SemanticOptions{},
		PageTextRequest{MaxBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "aé" || !got.Truncated {
		t.Fatalf("text=%q truncated=%v, want the cut backed up to a rune boundary", got.Text, got.Truncated)
	}
}

func TestExtractPageTextWithoutPDFToTextIsATypedCapabilityError(t *testing.T) {
	got, err := ExtractPageText(context.Background(), writeTempPDF(t), Capability{}, SemanticOptions{}, PageTextRequest{})
	if !errors.Is(err, ErrToolUnavailable) {
		t.Fatalf("err = %v, want ErrToolUnavailable", err)
	}
	if got.Text != "" {
		t.Fatalf("unexpected text on a capability error: %+v", got)
	}
}

func TestExtractPageTextKillsAHungToolAtTheTimeout(t *testing.T) {
	path := writeTempPDF(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	tool := fakeTool(t, `echo $$ > '`+pidFile+`'; sleep 30`)
	started := time.Now()
	_, err := ExtractPageText(context.Background(), path, Capability{PDFToText: tool},
		SemanticOptions{Timeout: 100 * time.Millisecond}, PageTextRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("hung tool held the call for %s", elapsed)
	}
	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("the fake tool never started: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatal(convErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hung tool pid %d is still alive after the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
