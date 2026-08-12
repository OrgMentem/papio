// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/pulse"
)

// pulseJSON is one structured object. The optional Available field is emitted
// only for the explicit old-daemon result; a supported daemon emits the pulse
// fields directly rather than wrapping them in a list envelope.
type pulseJSON struct {
	Available *bool `json:"available,omitempty"`
	*protocol.WorkPulseResponsePayload
}

func newPulseCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "pulse",
		Short:       "Show the daemon's current work pulse",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result api.PulseResult
			err := opt.call(cmd.Context(), "work.pulse_v1", map[string]any{
				"request_id":      job.NewID("pulse"),
				"schema_versions": []int{1},
			}, &result)
			if err != nil {
				if isUnknownMethod(err) {
					return renderUnavailablePulse(opt, opt.jsonOutput)
				}
				return err
			}
			if opt.jsonOutput {
				payload := protocol.WorkPulseResponsePayload(result)
				return opt.printJSON(pulseJSON{WorkPulseResponsePayload: &payload})
			}
			return renderPulseText(opt.out, pulse.Snapshot(result))
		},
	}
}

func renderUnavailablePulse(opt *options, jsonOutput bool) error {
	if jsonOutput {
		available := false
		return opt.printJSON(pulseJSON{Available: &available})
	}
	_, err := fmt.Fprintln(opt.out, "Unknown · live progress unavailable with this daemon")
	return err
}
func renderPulseText(w io.Writer, s pulse.Snapshot) error {
	parts := []string{pulse.PrimaryLabel(s)}
	add := func(label string, value *int64) {
		if value != nil {
			parts = append(parts, fmt.Sprintf("%s %d", label, *value))
		}
	}
	add("in flight", s.InFlight)
	add("continuing", s.Continuing)
	add("scheduled", s.Scheduled)
	add("waiting on you", s.WaitingRequired)
	add("stalled", s.Stalled)
	if s.NextAction != nil {
		text := "next " + s.NextAction.Kind + " at " + s.NextAction.At
		if s.NextAction.Source != "" {
			text += " · " + s.NextAction.Source
		}
		parts = append(parts, text)
	}
	if s.LastFinishedAt != "" {
		parts = append(parts, "last finished "+s.LastFinishedAt)
	}
	_, err := fmt.Fprintln(w, strings.Join(parts, " · "))
	return err
}
