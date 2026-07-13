// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
)

const (
	// MinimumPayloadBytes rejects error pages and truncated downloads before they
	// reach any parser.
	MinimumPayloadBytes = 5_000
	eofSearchBytes      = 8_192
)

// ValidatePayload performs the non-parsing admission check. declaredMIME is
// retained as evidence only: a content type header cannot make non-PDF bytes a
// PDF. In particular the magic header must begin at byte zero.
func ValidatePayload(body []byte, declaredMIME string) PayloadReport {
	r := PayloadReport{
		SizeBytes:   int64(len(body)),
		HasHeader:   bytes.HasPrefix(body, []byte("%PDF-")),
		HasEOF:      hasTailEOF(body),
		SniffedMIME: http.DetectContentType(body),
	}
	if len(body) < MinimumPayloadBytes {
		r.Reason = "payload is below the 5000-byte PDF minimum"
		return r
	}
	if !r.HasHeader {
		r.Reason = "payload does not begin with %PDF-"
		return r
	}
	// EOF is deliberately optional: incremental and streaming producers may
	// omit it. If present, however, it must be near the actual end. This
	// rejects a PDF prefix followed by an arbitrary appended payload.
	if bytes.Contains(body, []byte("%%EOF")) && !r.HasEOF {
		r.Reason = "%%EOF occurs before the final 8192 bytes"
		return r
	}
	// A declared MIME type is only corroborating evidence. Do not reject a
	// byte-valid PDF solely because a server mislabeled it.
	_ = declaredMIME
	r.OK = true
	return r
}

// ValidatePayloadFile applies the same cheap gate without reading an entire
// potentially 100 MiB artifact into memory. It reads only the sniff/header and
// tail windows, scanning in fixed-size chunks solely when it must distinguish
// an absent EOF marker from an early appended-payload marker.
func ValidatePayloadFile(path, declaredMIME string) (PayloadReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return PayloadReport{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return PayloadReport{}, err
	}
	if !info.Mode().IsRegular() {
		return PayloadReport{}, errors.New("PDF payload is not a regular file")
	}
	size := info.Size()
	head := make([]byte, min(int64(512), size))
	if len(head) > 0 {
		if _, err := f.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
			return PayloadReport{}, err
		}
	}
	tailSize := min(int64(eofSearchBytes), size)
	tail := make([]byte, tailSize)
	if len(tail) > 0 {
		if _, err := f.ReadAt(tail, size-tailSize); err != nil && !errors.Is(err, io.EOF) {
			return PayloadReport{}, err
		}
	}
	r := PayloadReport{
		SizeBytes: size, HasHeader: bytes.HasPrefix(head, []byte("%PDF-")),
		HasEOF: bytes.Contains(tail, []byte("%%EOF")), SniffedMIME: http.DetectContentType(head),
	}
	if size < MinimumPayloadBytes {
		r.Reason = "payload is below the 5000-byte PDF minimum"
		return r, nil
	}
	if !r.HasHeader {
		r.Reason = "payload does not begin with %PDF-"
		return r, nil
	}
	if !r.HasEOF {
		hasAny, err := fileContains(f, []byte("%%EOF"))
		if err != nil {
			return PayloadReport{}, err
		}
		if hasAny {
			r.Reason = "%%EOF occurs before the final 8192 bytes"
			return r, nil
		}
	}
	_ = declaredMIME
	r.OK = true
	return r, nil
}

func fileContains(f *os.File, needle []byte) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	buf := make([]byte, 64<<10)
	carry := make([]byte, 0, len(needle)-1)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			if bytes.Contains(window, needle) {
				return true, nil
			}
			keep := min(len(needle)-1, len(window))
			carry = append(carry[:0], window[len(window)-keep:]...)
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// PayloadGate is an alias retained for call sites that name the pipeline
// stage rather than its validation action.
func PayloadGate(body []byte, declaredMIME string) PayloadReport {
	return ValidatePayload(body, declaredMIME)
}

func hasTailEOF(body []byte) bool {
	start := len(body) - eofSearchBytes
	if start < 0 {
		start = 0
	}
	return bytes.Contains(body[start:], []byte("%%EOF"))
}
