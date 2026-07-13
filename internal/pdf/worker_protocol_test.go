// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"strings"
	"testing"
)

func TestStructuralParentRejectsWorkerPageCapViolation(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":11}'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{MaxPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid || !strings.Contains(report.Reason, "exceeds cap") {
		t.Fatalf("report=%+v", report)
	}
}
