// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMacOSUsesBoundedEscapedArgvAndSwallowsFailure(t *testing.T) {
	var name string
	var args []string
	var deadline time.Time
	sender := MacOS{Exec: func(ctx context.Context, gotName string, gotArgs ...string) error {
		name = gotName
		args = append([]string(nil), gotArgs...)
		deadline, _ = ctx.Deadline()
		return errors.New("notifications unavailable")
	}}
	before := time.Now()
	sender.Send(context.Background(), "A \"quoted\" path \\ here\nnext line")
	if name != "osascript" {
		t.Fatalf("command = %q", name)
	}
	want := []string{"-e", `display notification "A \"quoted\" path \\ here\nnext line" with title "papio"`}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	if deadline.IsZero() || deadline.Before(before.Add(4*time.Second)) || deadline.After(before.Add(6*time.Second)) {
		t.Fatalf("deadline = %v, want roughly five seconds after %v", deadline, before)
	}
}

// Coalescing is durable and is tested through Router and its ledger; no
// process-local Coalescer remains.
