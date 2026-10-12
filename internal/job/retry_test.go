// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"errors"
	"strings"
	"testing"
)

// A refused retry names the command that does apply to the state, so the
// operator is not left at a dead end.
func TestCheckRetryableNamesTheCommandForARefusedState(t *testing.T) {
	for state, want := range map[string]string{
		StateCancelled:     "papio acquire",
		StateAwaitingHuman: "papio actions open",
		StateNeedsReview:   "papio actions resolve",
	} {
		err := CheckRetryable(state)
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), want) {
			t.Fatalf("CheckRetryable(%s) = %v; want a conflict naming %q", state, err, want)
		}
	}
	for _, state := range []string{StateRetryWait, StateFailed, StateUnavailable} {
		if err := CheckRetryable(state); err != nil {
			t.Fatalf("CheckRetryable(%s) = %v; want nil", state, err)
		}
	}
}
