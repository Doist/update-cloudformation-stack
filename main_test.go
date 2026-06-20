package main

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

func Test_parseKvs(t *testing.T) {
	for _, tc := range []struct {
		input       []string
		pairsParsed int
		wantErr     bool
	}{
		{input: nil},
		{input: []string{"\n"}},
		{input: []string{"k=v"}, pairsParsed: 1},
		{input: []string{"k=v", "k=v"}, wantErr: true},
		{input: []string{"k=v", "k2=v"}, pairsParsed: 2},
		{input: []string{"k=v", "", "k2=v", ""}, pairsParsed: 2},
		{input: []string{"k=v", "k2=v", "k=v"}, wantErr: true},
		{input: []string{"k=v", "junk"}, wantErr: true},
		{input: []string{"k= ", "k2=v"}, pairsParsed: 2},
	} {
		got, err := parseKvs(tc.input)
		if tc.wantErr != (err != nil) {
			t.Errorf("input: %q, want error: %v, got error: %v", tc.input, tc.wantErr, err)
		}
		if l := len(got); l != tc.pairsParsed {
			t.Errorf("input: %q, got %d kv pairs, want %d", tc.input, l, tc.pairsParsed)
		}
	}
}

func Test_isFailure(t *testing.T) {
	for _, tc := range []struct {
		status types.ResourceStatus
		reason string
		want   bool
	}{
		{status: types.ResourceStatusUpdateFailed, reason: "S3 bucket already exists", want: true},
		{status: types.ResourceStatusCreateFailed, reason: "Invalid value", want: true},
		{status: "DELETE_FAILED", reason: "resource in use", want: true},
		{status: types.ResourceStatusUpdateFailed, reason: "Resource update cancelled", want: false},
		{status: types.ResourceStatusUpdateFailed, reason: "Resource creation Cancelled", want: false},
		{status: types.ResourceStatusUpdateComplete, reason: "", want: false},
		{status: types.ResourceStatusUpdateInProgress, reason: "", want: false},
	} {
		if got := isFailure(tc.status, tc.reason); got != tc.want {
			t.Errorf("isFailure(%q, %q) = %v, want %v", tc.status, tc.reason, got, tc.want)
		}
	}
}

func Test_sortedFailures(t *testing.T) {
	t0 := time.Unix(1000, 0)
	m := map[string]failedEvent{
		"b": {logicalID: "B", timestamp: t0.Add(2 * time.Minute)},
		"a": {logicalID: "A", timestamp: t0},
		"c": {logicalID: "C", timestamp: t0.Add(time.Minute)},
	}
	got := sortedFailures(m)
	want := []string{"A", "C", "B"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].logicalID != id {
			t.Errorf("position %d: got %q, want %q (root cause must be earliest)", i, got[i].logicalID, id)
		}
	}
}

func Test_mdCell(t *testing.T) {
	if got, want := mdCell("a | b\nc"), `a \| b c`; got != want {
		t.Errorf("mdCell() = %q, want %q", got, want)
	}
}

func Test_eventsConsoleURL(t *testing.T) {
	got := eventsConsoleURL("eu-west-1", "arn:aws:cloudformation:eu-west-1:1:stack/foo/abc")
	want := "https://eu-west-1.console.aws.amazon.com/cloudformation/home?region=eu-west-1#/stacks/events?stackId=arn%3Aaws%3Acloudformation%3Aeu-west-1%3A1%3Astack%2Ffoo%2Fabc"
	if got != want {
		t.Errorf("eventsConsoleURL() =\n  %q\nwant\n  %q", got, want)
	}
}
