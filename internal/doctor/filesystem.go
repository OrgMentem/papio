// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

// privacyError keeps the readiness finding separate from its platform-specific
// remedy. In particular Windows permissions cannot be repaired with chmod.
type privacyError struct {
	detail      string
	remediation string
}

func (e *privacyError) Error() string { return e.remediation }
