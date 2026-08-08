package app

import (
	"context"
	"net/http"
	"net/http/httptrace"

	"papio/internal/illiad"
)

// tracedSubmissionClient turns the net/http write boundary into a fail-closed
// ILLiad create classification. A response or any observed write attempt is
// ambiguous; only a Do error with no write callback and no response is
// provably pre-send.
type tracedSubmissionClient struct {
	base  illiad.HTTPClient
	class illiad.FailureClass
}

func newTracedSubmissionClient(base illiad.HTTPClient) *tracedSubmissionClient {
	return &tracedSubmissionClient{base: base, class: illiad.FailureAmbiguous}
}

func (c *tracedSubmissionClient) Do(req *http.Request) (*http.Response, error) {
	wrote := false
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote = true }}
	resp, err := c.base.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if resp != nil {
		c.class = illiad.FailureAmbiguous
	}
	if err == nil {
		return resp, nil
	}
	c.class = illiad.FailureAmbiguous
	if resp == nil && !wrote {
		c.class = illiad.FailurePreSend
	}
	return resp, &illiad.FailureError{Class: c.class, Err: err}
}

func (s *Service) submissionFailureClass(ctx context.Context, jobID string, requestID int64) (illiad.FailureClass, bool, error) {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return illiad.FailureAmbiguous, false, err
	}
	class := illiad.FailureAmbiguous
	found := false
	for _, event := range events {
		if event["kind"] != "delivery.submission_failure_classified" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		deliveryID, _ := detail["delivery_request_id"].(float64)
		if int64(deliveryID) != requestID {
			continue
		}
		found = true
		if value, ok := detail["class"].(string); ok {
			class = illiad.FailureClass(value)
		}
	}
	return class, found, nil
}
