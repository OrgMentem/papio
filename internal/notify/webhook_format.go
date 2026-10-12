// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"papio/internal/config"
)

// maxFormatKind bounds the event kind echoed into a chat title or header.
const maxFormatKind = 64

// FormatName returns the effective request format: the configured Format, or
// config.WebhookFormatJSON when none is set.
func (w *Webhook) FormatName() string {
	if w.Format == "" {
		return config.WebhookFormatJSON
	}
	return w.Format
}

// deliveryKeyHeader carries the stable per-row delivery key for the chat
// formats, whose bodies have no field for it. The json format keeps the key
// in its body.
const deliveryKeyHeader = "X-Papio-Delivery-Key"

// render builds the request body and headers for one event in the sender's
// configured format. The caller adds Authorization. An unknown format is an
// error rather than a fallback to json, so a bad value cannot post a shape
// the receiver misreads.
func (w *Webhook) render(event Event, sentAt time.Time) ([]byte, http.Header, error) {
	header := http.Header{}
	switch w.Format {
	case "", config.WebhookFormatJSON:
		body, err := json.Marshal(struct {
			Source string `json:"source"`
			Event
			SentAt string `json:"sent_at"`
		}{Source: "papio", Event: event, SentAt: sentAt.UTC().Format(time.RFC3339)})
		if err != nil {
			return nil, nil, err
		}
		header.Set("Content-Type", "application/json")
		return body, header, nil
	case config.WebhookFormatNtfy:
		// ntfy publishes a plain-text body; Title, Tags and Priority are
		// request headers (https://docs.ntfy.sh/publish/).
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("Title", chatTitle(event))
		header.Set("Tags", ntfyTags(event))
		header.Set("Priority", ntfyPriority(event))
		if event.DeliveryKey != "" {
			header.Set(deliveryKeyHeader, event.DeliveryKey)
		}
		return []byte(event.Message), header, nil
	case config.WebhookFormatSlack:
		// A Slack incoming webhook takes {"text": ...} in mrkdwn, where
		// &, < and > start control sequences and must be escaped.
		body, err := json.Marshal(struct {
			Text string `json:"text"`
		}{Text: "*" + slackEscape(chatTitle(event)) + "*\n" + slackEscape(event.Message)})
		if err != nil {
			return nil, nil, err
		}
		header.Set("Content-Type", "application/json")
		if event.DeliveryKey != "" {
			header.Set(deliveryKeyHeader, event.DeliveryKey)
		}
		return body, header, nil
	default:
		return nil, nil, fmt.Errorf("webhook: unknown format %q", w.Format)
	}
}

// chatTitle is the one-line heading the chat formats put above Message:
// "papio", then ": <kind>" when the event has a kind, then " (<count>)" when
// it stands for more than one item.
func chatTitle(event Event) string {
	title := "papio"
	if kind := formatKind(event.Kind); kind != "" {
		title += ": " + kind
	}
	if event.Count > 1 {
		title += " (" + strconv.Itoa(event.Count) + ")"
	}
	return title
}

// formatKind reduces a producer-owned event kind to header-safe ASCII. Kinds
// are dotted identifiers today; anything else is dropped rather than escaped.
func formatKind(kind string) string {
	var out strings.Builder
	for _, r := range kind {
		if out.Len() >= maxFormatKind {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == ':':
			out.WriteRune(r)
		}
	}
	return out.String()
}

func ntfyTags(event Event) string {
	if kind := formatKind(event.Kind); kind != "" {
		return "papio," + kind
	}
	return "papio"
}

// ntfyPriority marks the events that wait on a person as high; every other
// event uses ntfy's default.
func ntfyPriority(event Event) string {
	switch event.Kind {
	case "action.opened", "action.reminder":
		return "high"
	}
	return "default"
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func slackEscape(value string) string { return slackEscaper.Replace(value) }
