// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DesktopPresenceCapabilities are the zotio commands papio uses to learn
// whether Zotero desktop runs and to wait until it does. They are optional:
// RequiredCapabilities does not name them, so an older zotio still passes
// preflight and papio keeps its spaced import retries instead of waiting.
var DesktopPresenceCapabilities = []string{"desktop status", "desktop wait"}

// ErrDesktopPresenceUnsupported means the installed zotio cannot report
// Zotero desktop presence. Callers fall back to attempting the import.
var ErrDesktopPresenceUnsupported = errors.New("zotio does not report Zotero desktop presence")

// DesktopWaitTimeout bounds one `zotio desktop wait`. The caller restarts the
// wait when it elapses, so the value only sets how often an idle daemon
// spawns a fresh waiter; it is not a polling interval, because the waiter
// blocks on filesystem notifications in between.
const DesktopWaitTimeout = time.Hour

// desktopWaitGrace is how long papio lets zotio report its own timeout before
// it kills the waiter itself.
const desktopWaitGrace = time.Minute

const (
	desktopPresenceUnknown int32 = iota
	desktopPresenceSupported
	desktopPresenceUnsupported
)

// DesktopStatus is Zotero desktop's presence as `zotio desktop status --agent`
// and `zotio desktop wait --agent` report it: one bare JSON object. papio
// reads only the fields it acts on.
type DesktopStatus struct {
	// Running is true while a Zotero process holds its profile lock or its
	// connector answers.
	Running bool `json:"running"`
	// ConnectorReachable is true when the connector answered its ping; a
	// connector save needs this, not merely a running process.
	ConnectorReachable bool `json:"connector_reachable"`
	// State is "ready", "starting", "busy", "unresponsive", "connector_off",
	// or "stopped".
	State    string `json:"state,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Outcome is set only by `desktop wait`: "ready", "timeout", or a
	// failure such as "no_profile".
	Outcome string `json:"outcome,omitempty"`
}

// Ready reports whether a connector save can reach Zotero now. A desktop that
// runs but does not answer yet is still starting, and the import would fail.
func (s DesktopStatus) Ready() bool { return s.ConnectorReachable }

// Desktop states zotio reports for a Zotero that runs past its startup window
// with a connector that cannot take requests. Waiting does not end them;
// the operator must restart Zotero or turn its connector on.
const (
	DesktopStateUnresponsive = "unresponsive"
	DesktopStateConnectorOff = "connector_off"
)

// DesktopStateBusy is a Zotero past its startup window whose connector port
// accepted a connection and did not answer one check. A large sync can hold
// Zotero's main thread for seconds, so this is not a hang and waiting ends
// it: `zotio desktop wait` keeps checking, and reports unresponsive only
// after the connector stays silent for 60 seconds.
const DesktopStateBusy = "busy"

// Stuck reports whether Zotero runs but its connector will not recover on its
// own: it accepts connections and does not answer (unresponsive), or nothing
// listens on its port (connector_off).
func (s DesktopStatus) Stuck() bool {
	return !s.Ready() && (s.State == DesktopStateUnresponsive || s.State == DesktopStateConnectorOff)
}

// ImportNeedsDesktop reports whether an import for a job must reach Zotero
// desktop's connector. It derives from the same route choice PlanJobs makes,
// so a route change moves both together: a route of "connector" fails
// outright while Zotero is closed, and a linked file gets no route and needs
// no desktop.
func ImportNeedsDesktop(zotioItemKey, attachmentMode string) bool {
	mode := "stored"
	if strings.TrimSpace(attachmentMode) == "linked-file" {
		mode = "linked-file"
	}
	route := newItemRoute(mode)
	if strings.TrimSpace(zotioItemKey) != "" {
		route = existingItemRoute(mode)
	}
	return route == "connector"
}

// recordDesktopPresence caches whether a capability registry advertises the
// presence commands. Preflight calls it on every plan, so an upgraded zotio is
// noticed without a daemon restart.
func (c *Client) recordDesktopPresence(seen map[string]Capability) {
	for _, path := range DesktopPresenceCapabilities {
		if _, ok := seen[path]; !ok {
			c.desktopPresence.Store(desktopPresenceUnsupported)
			return
		}
	}
	c.desktopPresence.Store(desktopPresenceSupported)
}

// desktopPresenceSupported reads the registry once when nothing has cached the
// answer yet. A known answer costs no subprocess.
func (c *Client) desktopPresenceSupported(ctx context.Context) (bool, error) {
	switch c.desktopPresence.Load() {
	case desktopPresenceSupported:
		return true, nil
	case desktopPresenceUnsupported:
		return false, nil
	}
	out, err := c.run(ctx, "capabilities")
	if err != nil {
		return false, fmt.Errorf("zotio capabilities: %w", err)
	}
	capabilities, err := decodeCapabilities(out)
	if err != nil {
		return false, fmt.Errorf("decoding zotio capabilities: %w", err)
	}
	seen := make(map[string]Capability, len(capabilities))
	for _, capability := range capabilities {
		seen[capability.Path] = capability
	}
	c.recordDesktopPresence(seen)
	return c.desktopPresence.Load() == desktopPresenceSupported, nil
}

// DesktopStatus asks zotio once whether Zotero desktop runs. It returns
// ErrDesktopPresenceUnsupported for a zotio without the presence commands.
func (c *Client) DesktopStatus(ctx context.Context) (DesktopStatus, error) {
	supported, err := c.desktopPresenceSupported(ctx)
	if err != nil {
		return DesktopStatus{}, err
	}
	if !supported {
		return DesktopStatus{}, ErrDesktopPresenceUnsupported
	}
	out, err := c.run(ctx, "desktop", "status", "--agent")
	if err != nil {
		return DesktopStatus{}, err
	}
	return decodeDesktopStatus(out)
}

// WaitForDesktop blocks until zotio reports that Zotero desktop accepts
// connector saves, zotio's own timeout elapses, or ctx ends. zotio waits on
// filesystem notifications, so this costs one sleeping process rather than a
// poll, and it answers at once when the connector already accepts saves. A
// timeout returns a status that is not Ready and a nil error; the caller
// restarts the wait. A Zotero that runs but is stuck (DesktopStatus.Stuck)
// returns at once with a nil error. Any other exit, such as zotio finding no
// Zotero profile, is an error, so the caller backs off instead of respawning in
// a loop.
//
// The waiter gets a stdin pipe that stays open for its whole life and asks
// zotio to watch it: if the daemon dies without cancelling ctx, the pipe
// closes and zotio exits on its own, so no waiter outlives papio.
func (c *Client) WaitForDesktop(ctx context.Context) (DesktopStatus, error) {
	supported, err := c.desktopPresenceSupported(ctx)
	if err != nil {
		return DesktopStatus{}, err
	}
	if !supported {
		return DesktopStatus{}, ErrDesktopPresenceUnsupported
	}
	args := []string{"desktop", "wait", "--agent", "--watch-stdin", "--timeout", DesktopWaitTimeout.String()}
	ctx, cancel := context.WithTimeout(ctx, DesktopWaitTimeout+desktopWaitGrace)
	defer cancel()
	var out []byte
	if c.Exec != nil {
		out, err = c.Exec(ctx, args...)
	} else {
		out, err = c.runWaiter(ctx, args)
	}
	if ctx.Err() != nil {
		return DesktopStatus{}, fmt.Errorf("zotio desktop wait: %w", ctx.Err())
	}
	status, decodeErr := decodeDesktopStatus(out)
	switch {
	case err == nil && decodeErr == nil:
		return status, nil
	case err == nil:
		return DesktopStatus{}, decodeErr
	case decodeErr == nil && status.Outcome == "timeout":
		// zotio exits non-zero on its own timeout. The wait ended without
		// Zotero, which is an answer, not a failure.
		return status, nil
	case decodeErr == nil && status.Stuck():
		// zotio exits 15 at once when Zotero runs but its connector cannot
		// take requests. That is an answer too: the caller tells the
		// operator and waits again later.
		return status, nil
	case decodeErr == nil && status.Outcome != "":
		return DesktopStatus{}, fmt.Errorf("%w (outcome %s)", err, SanitizeErrorHint(status.Outcome))
	default:
		return DesktopStatus{}, err
	}
}

// runWaiter runs the long-lived waiter outside run's per-command timeout.
func (c *Client) runWaiter(ctx context.Context, args []string) ([]byte, error) {
	if strings.TrimSpace(c.Executable) == "" {
		return nil, errors.New("zotio executable is not configured")
	}
	path, err := exec.LookPath(c.Executable)
	if err != nil {
		return nil, fmt.Errorf("locating %q: %w", c.Executable, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = withoutEnv(os.Environ(), "ZOTERO_GROUP")
	// Bound Wait if a grandchild keeps the output pipes open after the kill.
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = stdin.Close() }()
	var stdout, stderr boundedBuffer
	stdout.max = maxStdoutBytes
	stderr.max = maxStderrBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if stdout.overflow {
		return nil, fmt.Errorf("zotio stdout exceeds %d bytes", maxStdoutBytes)
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("zotio desktop wait: %s", detail)
	}
	return stdout.Bytes(), nil
}

// decodeDesktopStatus reads the bare presence object zotio prints. The running
// field is required, so an unrelated document never reads as "Zotero is
// closed".
func decodeDesktopStatus(raw []byte) (DesktopStatus, error) {
	var wire struct {
		Running *bool `json:"running"`
		DesktopStatus
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &wire); err != nil {
		return DesktopStatus{}, fmt.Errorf("decoding zotio desktop status: %w", err)
	}
	if wire.Running == nil {
		return DesktopStatus{}, errors.New("zotio desktop status carries no running field")
	}
	status := wire.DesktopStatus
	status.Running = *wire.Running
	return status, nil
}
