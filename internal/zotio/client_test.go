// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestPreflightRequiresVersionAndTypedCapabilities(t *testing.T) {
	client := &Client{Executable: "zotio"}
	client.Exec = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "version --agent":
			return []byte("zotio 1.2.3\n"), nil
		case "capabilities":
			capabilities := make([]Capability, 0, len(RequiredCapabilities))
			for path, operation := range RequiredCapabilities {
				capability := Capability{Path: path, Operation: operation}
				if path == "attachments add" {
					capability.WriteTarget = "web_api"
				}
				capabilities = append(capabilities, capability)
			}
			return json.Marshal(capabilities)
		default:
			t.Fatalf("unexpected argv %q", args)
			return nil, nil
		}
	}

	result, err := client.Preflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != "1.2.3" || len(result.Capabilities) != len(RequiredCapabilities) {
		t.Fatalf("preflight = %+v", result)
	}
}

func TestPreflightRejectsOldOrIncompleteZotio(t *testing.T) {
	old := &Client{Executable: "zotio", Exec: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("zotio 0.9.0\n"), nil
	}}
	if _, err := old.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "older") || !strings.Contains(err.Error(), old.Executable) {
		t.Fatalf("old preflight err = %v", err)
	}

	incomplete := &Client{Executable: "zotio"}
	incomplete.Exec = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte("zotio 1.0.0\n"), nil
		}
		return []byte(`[{"path":"items missing-pdf","operation":"read"}]`), nil
	}
	if _, err := incomplete.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "missing capability") {
		t.Fatalf("incomplete preflight err = %v", err)
	}
}

func TestMissingPDFUsesExactCollectionAndValidatesRows(t *testing.T) {
	var got []string
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte(`[{"key":"AB12CD34","title":"Paper","doi":"10.1000/test"}]`), nil
	}}
	items, err := client.MissingPDF(context.Background(), "ZX98YU76", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "AB12CD34" {
		t.Fatalf("items = %+v", items)
	}
	want := "--agent items missing-pdf --limit 25 --collection ZX98YU76"
	if strings.Join(got, " ") != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}

	for _, key := range []string{"../../bad", "ab12CD34", "AB12CD3", "AB12CD345"} {
		t.Run(key, func(t *testing.T) {
			client.Exec = func(_ context.Context, _ ...string) ([]byte, error) {
				return []byte(fmt.Sprintf(`[{"key":%q,"title":"Paper"}]`, key)), nil
			}
			if _, err := client.MissingPDF(context.Background(), "", 1); err == nil || !strings.Contains(err.Error(), "Zotio queue row 0 has invalid item key") {
				t.Fatalf("invalid Zotero queue key error = %v", err)
			}
		})
	}
}

func TestMissingPDFKeysUsesExactParentKeys(t *testing.T) {
	var got []string
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte(`[{"key":"AB12CD34","title":"Paper"}]`), nil
	}}
	items, err := client.MissingPDFKeys(context.Background(), []string{"AB12CD34", "EF56GH78"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "AB12CD34" {
		t.Fatalf("items = %+v", items)
	}
	if actual, want := strings.Join(got, " "), "--agent items missing-pdf --keys AB12CD34,EF56GH78"; actual != want {
		t.Fatalf("argv = %q, want %q", actual, want)
	}
}

func TestGetItemBuildsTitleAuthorYearFallback(t *testing.T) {
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "--agent items get AB12CD34" {
			t.Fatalf("argv = %q", args)
		}
		return []byte(`{
			"results": {
				"data": {
					"key": "AB12CD34",
					"title": "A paper without a DOI",
					"date": "Spring 2024",
					"collections": ["ZX98YU76"],
					"creators": [
						{"creatorType":"editor","firstName":"Ed","lastName":"Itor"},
						{"creatorType":"author","firstName":"Ada","lastName":"Lovelace"}
					]
				},
				"meta": {"parsedDate":"2024"}
			}
		}`), nil
	}}
	item, err := client.GetItem(context.Background(), "AB12CD34")
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "A paper without a DOI" || item.Year != 2024 || len(item.Authors) != 1 || item.Authors[0] != "Ada Lovelace" {
		t.Fatalf("item = %+v", item)
	}
}

func TestRunJSONPreservesStructuredOutputFromNonzeroCommand(t *testing.T) {
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"ok":false,"mode":"apply","result":{"summary":{"failed":1}}}`), errors.New("exit status 1")
	}}
	out, err := client.RunJSON(context.Background(), "--agent", "--yes", "attachments", "add")
	if err == nil || !strings.Contains(string(out), `"failed":1`) {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestSyncRequiresSuccessfulTerminalSummary(t *testing.T) {
	var got []string
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte("{\"event\":\"sync_start\",\"resource\":\"items\"}\n{\"event\":\"sync_summary\",\"success\":1,\"errored\":0}\n"), nil
	}}
	if err := client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "--agent sync" {
		t.Fatalf("argv = %q", got)
	}

	client.Exec = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("{\"event\":\"sync_summary\",\"success\":0,\"errored\":1}\n"), nil
	}
	if err := client.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "1 errored") {
		t.Fatalf("errored sync = %v", err)
	}

	client.Exec = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("{\"event\":\"sync_complete\",\"resource\":\"items\"}\n"), nil
	}
	if err := client.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "did not report a summary") {
		t.Fatalf("summary-less sync = %v", err)
	}
}

func TestBoundedBufferReportsOverflowWithoutShortWrite(t *testing.T) {
	buffer := boundedBuffer{max: 4}
	input := []byte("abcdef")
	n, err := buffer.Write(input)
	if err != nil || n != len(input) || !buffer.overflow || buffer.String() != "abcd" {
		t.Fatalf("write = (%d, %v), buffer=%q overflow=%v", n, err, buffer.String(), buffer.overflow)
	}
}

func TestRunCanceledContextClassifiesAsCancelNotTimeout(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A real exec path is required: when c.Exec is set run() returns before the
	// ctx.Err() branch, so this drives the production LookPath/CommandContext
	// code that must distinguish cancellation from a genuine timeout.
	client := &Client{Executable: self}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, runErr := client.RunJSON(ctx, "--agent", "noop")
	if runErr == nil {
		t.Fatal("canceled context returned no error")
	}
	if !strings.Contains(runErr.Error(), "zotio command canceled") || strings.Contains(runErr.Error(), "timed out") {
		t.Fatalf("run error = %v, want cancel wording not timeout", runErr)
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("run error does not wrap context.Canceled: %v", runErr)
	}
	if info := ErrorInfoFrom(runErr); info.Class != ErrorClassZotioCanceled {
		t.Fatalf("class = %q, want %q", info.Class, ErrorClassZotioCanceled)
	}
}

func TestRunJSONPreservesEnvelopeOnCancelError(t *testing.T) {
	envelope := `{"ok":false,"mode":"apply","result":{"summary":{"applied":1}}}`
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(envelope), fmt.Errorf("zotio command canceled: %w", context.Canceled)
	}}
	out, err := client.RunJSON(context.Background(), "--agent", "--yes", "import", "apply")
	if err == nil {
		t.Fatal("expected non-nil run error alongside preserved envelope")
	}
	if !strings.Contains(string(out), `"applied":1`) {
		t.Fatalf("envelope not preserved on cancel: out=%s", out)
	}
	if info := ErrorInfoFrom(err); info.Class != ErrorClassZotioCanceled {
		t.Fatalf("class = %q, want %q", info.Class, ErrorClassZotioCanceled)
	}
}
