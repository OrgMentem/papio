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

type recordingSender struct{ messages []string }

func (s *recordingSender) Send(_ context.Context, message string) {
	s.messages = append(s.messages, message)
}

func TestCoalescerSummarizesEachNotificationClass(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	recorded := &recordingSender{}
	coalescer := NewCoalescer(recorded)
	coalescer.Now = func() time.Time { return now }
	coalescer.after = func(time.Duration, func()) {}

	coalescer.HumanAction(context.Background())
	now = now.Add(10 * time.Second)
	coalescer.HumanAction(context.Background())
	now = now.Add(10 * time.Second)
	coalescer.HumanAction(context.Background())
	coalescer.Imported(context.Background())
	now = now.Add(41 * time.Second)
	coalescer.HumanAction(context.Background())

	want := []string{
		"1 paper needs your attention; run papio status to see why",
		"1 paper imported",
		"3 papers need your attention; run papio status to see why",
	}
	if !reflect.DeepEqual(recorded.messages, want) {
		t.Fatalf("messages = %#v, want %#v", recorded.messages, want)
	}
}

func TestCoalescerFlushesPendingNotificationsWhenWindowCloses(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	recorded := &recordingSender{}
	coalescer := NewCoalescer(recorded)
	coalescer.Now = func() time.Time { return now }
	var callbacks []func()
	coalescer.after = func(_ time.Duration, callback func()) {
		callbacks = append(callbacks, callback)
	}

	coalescer.Imported(context.Background())
	now = now.Add(10 * time.Second)
	coalescer.Imported(context.Background())
	if len(callbacks) != 1 {
		t.Fatalf("scheduled callbacks = %d, want 1", len(callbacks))
	}
	now = now.Add(50 * time.Second)
	callbacks[0]()

	want := []string{"1 paper imported", "1 paper imported"}
	if !reflect.DeepEqual(recorded.messages, want) {
		t.Fatalf("messages = %#v, want %#v", recorded.messages, want)
	}
}
