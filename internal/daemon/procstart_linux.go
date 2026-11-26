// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// platformProcessStart reads when the process with pid began, in clock ticks
// after boot, from field 22 of /proc/<pid>/stat.
func platformProcessStart(pid int) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parseStatStart(data)
}

// parseStatStart extracts the start time from the text of a /proc/<pid>/stat
// line. The command name, field 2, is parenthesized and may hold spaces and
// parentheses, so the fields that follow it start after the last ")".
func parseStatStart(stat []byte) (uint64, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	// Field 3, the state, is fields[0]; field 22 is fields[19].
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return 0, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, err == nil
}
