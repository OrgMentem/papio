// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/browser"
)

// browserSessionsResult mirrors the browser.sessions RPC response.
type browserSessionsResult struct {
	Sessions     []browser.SessionSummary `json:"sessions"`
	DeniedHellos int                      `json:"denied_hellos"`
	Takeovers    int                      `json:"takeovers"`
}

func newBrowserCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "browser", Short: "Inspect and switch connected browser sessions"}

	sessions := &cobra.Command{
		Use:         "sessions",
		Short:       "List browser sessions connected since daemon start",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result browserSessionsResult
			if err := opt.call(cmd.Context(), "browser.sessions", map[string]any{}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if len(result.Sessions) == 0 {
				_, err := fmt.Fprintln(opt.out, "no browser has connected since daemon start")
				return err
			}
			for _, session := range result.Sessions {
				role := "pending"
				if session.Holder {
					role = "holder"
				}
				if _, err := fmt.Fprintf(opt.out, "%s\t%s\tv%s\tlast sync %s\n",
					shortSessionID(session.ID), role, session.ExtensionVersion, sessionAge(session.LastSyncAt)); err != nil {
					return err
				}
			}
			return nil
		},
	}

	var latest bool
	use := &cobra.Command{
		Use:   "use [session-id]",
		Short: "Give one browser session the papio offer/handoff flow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == latest {
				return errors.New("pass exactly one of a session id or --latest")
			}
			target := ""
			if len(args) == 1 {
				target = args[0]
			} else {
				var result browserSessionsResult
				if err := opt.call(cmd.Context(), "browser.sessions", map[string]any{}, &result); err != nil {
					return err
				}
				for _, session := range result.Sessions {
					if !session.Holder {
						target = session.ID // sessions are ordered newest-sync first
						break
					}
				}
				if target == "" {
					return errors.New("no other browser session to switch to")
				}
			}
			var result struct {
				Claimed   bool   `json:"claimed"`
				SessionID string `json:"session_id"`
			}
			if err := opt.call(cmd.Context(), "browser.claim", map[string]string{"session_id": target}, &result); err != nil {
				return err
			}
			resolved := result.SessionID
			if resolved == "" {
				resolved = target
			}
			return opt.printResult(result, "browser session %s now holds the papio session", shortSessionID(resolved))
		},
	}
	use.Flags().BoolVar(&latest, "latest", false, "switch to the most recently active pending session")

	command.AddCommand(sessions, use)
	return command
}

func shortSessionID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// sessionAge renders an RFC3339 timestamp as a coarse relative age.
func sessionAge(stamp string) string {
	parsed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	age := time.Since(parsed)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	}
}
