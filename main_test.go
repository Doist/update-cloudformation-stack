package main

import (
	"os"
	"path/filepath"
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

func Test_updateFailed(t *testing.T) {
	const link = "https://console.aws.amazon.com/go/view?arn=arn%3Aaws%3Acloudformation%3Aeu-west-1%3A1%3Astack%2Ffoo%2Fabc"
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	e := &updateFailed{
		stackARN: "arn:aws:cloudformation:eu-west-1:1:stack/foo/abc",
		status:   types.ResourceStatusUpdateRollbackComplete,
		failures: []failedEvent{
			{logicalID: "Bucket", resType: "AWS::S3::Bucket", status: types.ResourceStatusUpdateFailed, reason: "bucket | already\nexists", timestamp: t0},
			{logicalID: "Role", resType: "AWS::IAM::Role", status: types.ResourceStatusUpdateFailed, timestamp: t0.Add(time.Minute)},
		},
	}
	if got, want := e.Error(), "Bucket (AWS::S3::Bucket) UPDATE_FAILED: bucket | already exists, see stack events: "+link; got != want {
		t.Errorf("Error() =\n  %q\nwant\n  %q", got, want)
	}

	name := filepath.Join(t.TempDir(), "summary.md")
	if err := e.writeSummary(name); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	want := "## ❌ CloudFormation deployment failed (UPDATE_ROLLBACK_COMPLETE)\n\n" +
		"| Time (UTC) | Resource | Type | Status | Reason |\n" +
		"| --- | --- | --- | --- | --- |\n" +
		"| 10:00:00 | Bucket | AWS::S3::Bucket | UPDATE_FAILED | bucket \\| already exists |\n" +
		"| 10:01:00 | Role | AWS::IAM::Role | UPDATE_FAILED |  |\n\n" +
		"[View stack events in the AWS Console](" + link + ")\n"
	if string(got) != want {
		t.Errorf("writeSummary() wrote\n%s\nwant\n%s", got, want)
	}

	e.failures = nil
	if got, want := e.Error(), "UPDATE_ROLLBACK_COMPLETE, see stack events: "+link; got != want {
		t.Errorf("Error() with no failures =\n  %q\nwant\n  %q", got, want)
	}
}

func Test_mdCell(t *testing.T) {
	if got, want := mdCell("  a | b\n\tc "), `a \| b c`; got != want {
		t.Errorf("mdCell() = %q, want %q", got, want)
	}
}
