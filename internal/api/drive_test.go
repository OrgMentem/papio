// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"testing"

	"papio/internal/drive"
)

// The operator's pause is the stop button for the paced drive: it must reach
// the pacer, report the paused state, and come back off with resume.
func TestDriveRPCPausesAndResumesThePacer(t *testing.T) {
	system := testSystem(t)
	router := Router(system)

	var status drive.Status
	if rpcErr := callMethod(t, router, "drive.pause", struct{}{}, &status); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !status.Paused || status.PausedReason != drive.PauseOperator {
		t.Fatalf("drive.pause status = %+v; want paused by the operator", status)
	}
	status = drive.Status{}
	if rpcErr := callMethod(t, router, "drive.status", struct{}{}, &status); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !status.Paused {
		t.Fatalf("drive.status after pause = %+v; want paused", status)
	}
	status = drive.Status{}
	if rpcErr := callMethod(t, router, "drive.resume", struct{}{}, &status); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if status.Paused {
		t.Fatalf("drive.resume status = %+v; want not paused", status)
	}
}

func TestDriveRPCReportsAnUnavailableDriveAndRejectsParams(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	for _, method := range []string{"drive.status", "drive.pause", "drive.resume"} {
		if rpcErr := callMethod(t, router, method, map[string]string{"job_id": "x"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("%s with params = %+v; want invalid_argument", method, rpcErr)
		}
	}
	system.Drive = nil
	for _, method := range []string{"drive.status", "drive.pause", "drive.resume"} {
		if rpcErr := callMethod(t, router, method, struct{}{}, nil); rpcErr == nil || rpcErr.Code != "unavailable" {
			t.Fatalf("%s without a drive = %+v; want unavailable", method, rpcErr)
		}
	}
}
