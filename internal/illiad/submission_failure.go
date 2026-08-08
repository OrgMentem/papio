package illiad

import (
	"errors"
	"fmt"
)

// FailureClass describes whether a failed create request is safe to replay.
type FailureClass string

const (
	FailurePreSend   FailureClass = "pre_send"
	FailureAmbiguous FailureClass = "ambiguous"
)

// FailureError carries the transport boundary classification through the
// client's existing error chain. Callers must treat absent classification as
// ambiguous.
type FailureError struct {
	Class FailureClass
	Err   error
}

func (e *FailureError) Error() string {
	if e == nil || e.Err == nil {
		return fmt.Sprintf("illiad: classified request failure (%s)", e.Class)
	}
	return e.Err.Error()
}

func (e *FailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// FailureClassOf returns a trusted transport classification, if one exists.
// Missing or malformed classifications are deliberately not converted to
// pre-send: callers must fail closed to ambiguous.
func FailureClassOf(err error) (FailureClass, bool) {
	var failure *FailureError
	if !errors.As(err, &failure) || failure == nil {
		return FailureAmbiguous, false
	}
	if failure.Class != FailurePreSend && failure.Class != FailureAmbiguous {
		return FailureAmbiguous, false
	}
	return failure.Class, true
}
