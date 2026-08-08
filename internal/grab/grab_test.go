package grab

import (
	"context"
	"testing"

	"papio/internal/store"
)

func TestAllocateReturnsExistingNonterminalGrab(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	first, err := svc.Allocate(ctx, "pdf.example.org", "A paper")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(ctx, "pdf.example.org", "A paper")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second allocation id = %q, want existing %q", second.ID, first.ID)
	}
}
