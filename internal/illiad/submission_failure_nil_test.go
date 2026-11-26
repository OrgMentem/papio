package illiad

import "testing"

func TestFailureErrorNilReceiverDoesNotPanic(t *testing.T) {
	var e *FailureError
	if e.Error() == "" {
		t.Fatal("empty message")
	}
}
