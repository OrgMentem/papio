// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import "context"

// Event is a structured notification. Message is always populated and is the
// complete human-readable form; the remaining fields exist so remote receivers
// (webhooks) can route and filter without parsing prose. Senders that only
// understand text deliver Message and lose nothing essential.
type Event struct {
	// Kind names the event class, e.g. "watch.alert", "watch.backfill",
	// "watch.acquire", "notice". Empty means plain notice.
	Kind    string `json:"event,omitempty"`
	Message string `json:"message"`
	// WatchID and WatchLabel identify the originating watch for watch.* kinds.
	WatchID    int64  `json:"watch_id,omitempty"`
	WatchLabel string `json:"watch_label,omitempty"`
	// Count is the event's primary quantity: works queued or reported.
	Count int `json:"count,omitempty"`
}

// EventSender is optionally implemented by senders that can deliver the
// structured form. Plain Senders receive Event.Message via Send.
type EventSender interface {
	SendEvent(ctx context.Context, event Event)
}

// Emit delivers event through s, upgrading to the structured form when the
// sender supports it. A nil sender is a no-op, mirroring Sender call sites.
func Emit(ctx context.Context, s Sender, event Event) {
	if s == nil {
		return
	}
	if es, ok := s.(EventSender); ok {
		es.SendEvent(ctx, event)
		return
	}
	s.Send(ctx, event.Message)
}
