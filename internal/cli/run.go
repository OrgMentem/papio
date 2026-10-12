package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"

	"papio/internal/ipc"
)

// Error codes that --json failures report besides a daemon RemoteError code.
const (
	errorCodeDaemonUnreachable = "daemon_unreachable"
	errorCodeInvalidArgument   = "invalid_argument"
	errorCodeGeneric           = "error"
)

// usageError marks a cobra flag-parsing or argument-count failure. It keeps the
// original message, so plain output does not change.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// cobraUsagePrefixes name the argument-validation errors cobra returns as bare
// strings (cobra has no typed error for them).
var cobraUsagePrefixes = []string{
	"unknown flag",
	"unknown shorthand flag",
	"unknown command ",
	"accepts ",
	"requires at least ",
	"requires at most ",
	"invalid argument ",
	"required flag(s) ",
	"if any flags in the group ",
	"flag needs an argument",
	"unknown ", // custom "unknown <group> command %q; valid verbs: ..." of configureCommandGroups
}

// jsonErrorBody is the object under the "error" key of a --json failure.
type jsonErrorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Class      string `json:"class,omitempty"`
	Hint       string `json:"hint,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// classifyError maps err to the stable --json error object.
func classifyError(err error) jsonErrorBody {
	body := jsonErrorBody{Code: errorCodeGeneric, Message: err.Error()}
	var remote *ipc.RemoteError
	switch {
	case errors.As(err, &remote) && remote != nil:
		body.Message = remote.Message
		if remote.Code != "" {
			body.Code = remote.Code
		}
		if remote.Detail != nil {
			body.Class = remote.Detail.ErrorClass
			body.Hint = remote.Detail.ErrorHint
			body.HTTPStatus = remote.Detail.ErrorHTTPStatus
		}
	case errors.Is(err, ipc.ErrDaemonUnreachable):
		body.Code = errorCodeDaemonUnreachable
	case isUsageError(err):
		body.Code = errorCodeInvalidArgument
	}
	return body
}

func isUsageError(err error) bool {
	var usage *usageError
	if errors.As(err, &usage) {
		return true
	}
	message := err.Error()
	for _, prefix := range cobraUsagePrefixes {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
}

// countingWriter records whether anything reached the wrapped writer.
type countingWriter struct {
	w io.Writer
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// jsonRequested reports whether the invocation asked for --json. The last
// --json/--json=<bool> in args wins, matching cobra's last-flag-wins parse and
// covering a flag error raised before cobra reached --json. With no such
// argument (in-process callers), the parsed flag value decides.
func jsonRequested(root *cobra.Command, args []string) bool {
	found, value := false, false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			found, value = true, true
		} else if raw, ok := strings.CutPrefix(arg, "--json="); ok {
			if parsed, err := strconv.ParseBool(raw); err == nil {
				found, value = true, parsed
			}
		}
	}
	if found {
		return value
	}
	set, err := root.PersistentFlags().GetBool("json")
	return err == nil && set
}

// reportError writes the failure to errOut. In --json mode it writes one
// {"error":{...}} object unless the command already printed its structured
// result on stdout (wroteResult); those commands (doctor, bench, jobs cancel,
// ...) return a sentinel error after the result, and a second object would
// contradict it, so they keep the plain "papio: <error>" line.
func reportError(errOut io.Writer, err error, jsonMode, wroteResult bool) {
	if !jsonMode || wroteResult {
		_, _ = fmt.Fprintln(errOut, "papio:", err)
		return
	}
	encoder := json.NewEncoder(errOut)
	encoder.SetEscapeHTML(false)
	if encodeErr := encoder.Encode(map[string]jsonErrorBody{"error": classifyError(err)}); encodeErr != nil {
		_, _ = fmt.Fprintln(errOut, "papio:", err)
	}
}

// Run executes the papio command line with args and returns the process exit
// status (0 or 1).
func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	counted := &countingWriter{w: out}
	root := NewRoot(counted, errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	reportError(errOut, err, jsonRequested(root, args), counted.n.Load() > 0)
	return 1
}

// RunProcess runs the command line of this process.
func RunProcess(ctx context.Context) int {
	return Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
