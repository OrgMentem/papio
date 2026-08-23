// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"
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

	var resolveReason string
	resolvePermit := &cobra.Command{
		Use:   "resolve <permit-id>",
		Short: "Release one unknown-completion browser effect permit",
		Long:  "Release one exact unknown-completion effect permit after an operator has independently resolved whether its browser effect completed. This is break-glass recovery; it never releases a held permit.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason := strings.TrimSpace(resolveReason)
			if reason == "" {
				return errors.New("--reason is required")
			}
			var result struct {
				Resolved bool   `json:"resolved"`
				PermitID string `json:"permit_id"`
			}
			if err := opt.call(cmd.Context(), "browser.effect_permit.resolve", map[string]string{
				"permit_id": args[0], "reason": reason,
			}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "effect permit %s resolved", result.PermitID)
		},
	}
	resolvePermit.Flags().StringVar(&resolveReason, "reason", "", "operator reason recorded in the durable audit event")
	permit := &cobra.Command{
		Use:   "permit",
		Short: "Recover daemon-owned browser effect permits",
	}
	permit.AddCommand(resolvePermit)
	var reloadTimeout time.Duration
	reload := &cobra.Command{
		Use:   "reload",
		Short: "Reload the connected development-mode extension from disk",
		Long:  "Reload the connected development-mode extension from disk, replacing the manual chrome://extensions Reload click. It only affects an unpacked extension, because the extension refuses the command unless chrome.management.getSelf() reports installType \"development\". A new session id is the proof the new bundle is live.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			var before browserSessionsResult
			if err := opt.call(ctx, "browser.sessions", map[string]any{}, &before); err != nil {
				return err
			}
			var previous string
			for _, s := range before.Sessions {
				if s.Holder {
					previous = s.ID
					break
				}
			}
			if previous == "" {
				return errors.New("no browser session holds the bridge")
			}
			var result struct {
				SessionID string `json:"session_id"`
				ReloadID  string `json:"reload_id"`
			}
			if err := opt.call(ctx, "browser.dev_reload", map[string]any{}, &result); err != nil {
				return err
			}
			if reloadTimeout == 0 {
				return opt.printResult(result, "reload %s sent for browser session %s; not waiting for reconnect (--timeout=0)", result.ReloadID, shortSessionID(previous))
			}
			deadline := time.Now().Add(reloadTimeout)
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("browser session %s did not reconnect within %s: the extension did not reconnect with a new session; a store-installed extension refuses dev_reload by design — this is the expected outcome when the loaded extension is not unpacked", shortSessionID(previous), reloadTimeout)
				}
				var cur browserSessionsResult
				if err := opt.call(ctx, "browser.sessions", map[string]any{}, &cur); err != nil {
					return err
				}
				var current string
				for _, s := range cur.Sessions {
					if s.Holder {
						current = s.ID
						break
					}
				}
				if current != "" && current != previous {
					return opt.printResult(result, "browser session %s reloaded; %s now holds the papio session", shortSessionID(previous), shortSessionID(current))
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		},
	}
	reload.Flags().DurationVar(&reloadTimeout, "timeout", 15*time.Second, "how long to wait for the reloaded extension to reconnect (0 waits not at all)")

	command.AddCommand(sessions, use, permit, reload)
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
