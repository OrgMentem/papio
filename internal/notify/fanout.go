// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import "context"

// Fanout delivers notifications sequentially to each non-nil sender. Each
// sender handles its own failures, so one destination cannot prevent another.
func Fanout(senders ...Sender) Sender {
	fanout := make(senderFanout, 0, len(senders))
	for _, sender := range senders {
		if sender != nil {
			fanout = append(fanout, sender)
		}
	}
	return fanout
}

type senderFanout []Sender

func (f senderFanout) Send(ctx context.Context, message string) {
	for _, sender := range f {
		sender.Send(ctx, message)
	}
}
