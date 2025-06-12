package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
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
		{input: []string{"k= ", "k2=v"}, wantErr: true},
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

type mockCFNClient struct {
	statuses []types.StackStatus
	calls    int
	err      error
}

func (m *mockCFNClient) DescribeStacks(ctx context.Context, input *cloudformation.DescribeStacksInput, optFns ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.calls >= len(m.statuses) {
		return nil, errors.New("too many calls")
	}
	status := m.statuses[m.calls]
	m.calls++
	return &cloudformation.DescribeStacksOutput{
		Stacks: []types.Stack{
			{
				StackName:   input.StackName,
				StackStatus: status,
			},
		},
	}, nil
}

func Test_waitForStableStack(t *testing.T) {
	tests := []struct {
		name     string
		statuses []types.StackStatus
		wantErr  bool
	}{
		{
			name:     "already stable",
			statuses: []types.StackStatus{types.StackStatusUpdateComplete},
		},
		{
			name:     "transient in-progress, then stable",
			statuses: []types.StackStatus{types.StackStatusUpdateInProgress, types.StackStatusUpdateComplete},
		},
		{
			name:     "bad state immediately",
			statuses: []types.StackStatus{types.StackStatusUpdateRollbackFailed},
			wantErr:  true,
		},
		{
			name:     "never stable",
			statuses: []types.StackStatus{types.StackStatusUpdateInProgress, types.StackStatusUpdateInProgress, types.StackStatusUpdateInProgress},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCFNClient{statuses: tt.statuses}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			_, err := waitForStableStack(ctx, mock, "test-stack", 10*time.Millisecond, 100*time.Millisecond)
			if (err != nil) != tt.wantErr {
				t.Errorf("waitForStableStack() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
