package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/smithy-go"
)

func main() {
	log.SetFlags(0)
	var stackName string
	flag.StringVar(&stackName, "stack", stackName, "name of the CloudFormation stack to update")
	var express bool
	flag.BoolVar(&express, "express", express, "CloudFormation express mode")
	flag.Parse()
	if err := run(context.Background(), express, stackName, flag.Args()); err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "ValidationError" && ae.ErrorMessage() == "No updates are to be performed." {
			debugf("error: %v", err)
			log.Print(githubWarnPrefix, "nothing to update")
			return
		}
		var uf *updateFailed
		if name := os.Getenv("GITHUB_STEP_SUMMARY"); name != "" && errors.As(err, &uf) {
			if err := uf.writeSummary(name); err != nil {
				debugf("cannot write step summary: %v", err)
			}
		}
		log.Fatal(githubErrPrefix, err)
	}
}

func run(ctx context.Context, express bool, stackName string, args []string) error {
	if stackName == "" {
		return errors.New("stack name must be set")
	}
	if underGithub && len(args) == 0 {
		args = strings.Split(os.Getenv("INPUT_PARAMETERS"), "\n")
	}
	toReplace, err := parseKvs(args)
	if err != nil {
		return err
	}
	if len(toReplace) == 0 {
		return errors.New("empty parameters list")
	}
	debugf("loaded parameters: %v", toReplace)
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	svc := cloudformation.NewFromConfig(cfg)

	desc, err := svc.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackName})
	if err != nil {
		return err
	}
	if l := len(desc.Stacks); l != 1 {
		return fmt.Errorf("DescribeStacks returned %d stacks, expected 1", l)
	}
	stack := desc.Stacks[0]
	var params []types.Parameter
	for _, p := range stack.Parameters {
		k := unptr(p.ParameterKey)
		if v, ok := toReplace[k]; ok {
			params = append(params, types.Parameter{ParameterKey: &k, ParameterValue: &v})
			delete(toReplace, k)
			continue
		}
		params = append(params, types.Parameter{ParameterKey: &k, UsePreviousValue: new(true)})
	}
	if len(toReplace) != 0 {
		return fmt.Errorf("stack has no parameters with these names: %s", strings.Join(slices.Sorted(maps.Keys(toReplace)), ", "))
	}

	debugf("parameters to call UpdateStack with:")
	for _, p := range params {
		switch {
		case unptr(p.UsePreviousValue):
			debugf("%s (use the previous value)", unptr(p.ParameterKey))
		default:
			debugf("%s: %s", unptr(p.ParameterKey), unptr(p.ParameterValue))
		}
	}

	var depconf *types.DeploymentConfig
	if express {
		depconf = &types.DeploymentConfig{Mode: types.DeploymentConfigModeExpress, DisableRollback: new(false)}
	}
	token := newToken()
	_, err = svc.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName:           &stackName,
		ClientRequestToken:  &token,
		UsePreviousTemplate: new(true),
		Parameters:          params,
		Capabilities:        stack.Capabilities,
		NotificationARNs:    stack.NotificationARNs,
		DeploymentConfig:    depconf,
	})
	if err != nil {
		return err
	}
	log.Print("polling for stack updates until it's ready, this may take a while")
	oldEventsCutoff := time.Now().Add(-time.Hour)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Events come newest first. On failure the stack-level terminal event
		// shows up before the resource failures that caused it, so keep
		// scanning to collect them all before reporting.
		var terminal types.ResourceStatus
		var failures []failedEvent // newest first
		p := cloudformation.NewDescribeStackEventsPaginator(svc, &cloudformation.DescribeStackEventsInput{StackName: &stackName})
	scanEvents:
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, evt := range page.StackEvents {
				if evt.Timestamp != nil && unptr(evt.Timestamp).Before(oldEventsCutoff) {
					break scanEvents
				}
				if evt.ClientRequestToken == nil || *evt.ClientRequestToken != token {
					continue
				}
				debugf("%s\t%s\t%v", unptr(evt.ResourceType), unptr(evt.LogicalResourceId), evt.ResourceStatus)
				if unptr(evt.LogicalResourceId) == stackName && unptr(evt.ResourceType) == "AWS::CloudFormation::Stack" {
					switch evt.ResourceStatus {
					case types.ResourceStatusUpdateComplete:
						return nil
					case types.ResourceStatusUpdateRollbackComplete,
						types.ResourceStatusUpdateRollbackFailed,
						types.ResourceStatusRollbackFailed:
						terminal = evt.ResourceStatus
					case types.ResourceStatusUpdateInProgress:
						// the oldest event of this update, nothing to see past it
						break scanEvents
					}
				}
				reason := unptr(evt.ResourceStatusReason)
				if strings.HasSuffix(string(evt.ResourceStatus), "_FAILED") && !strings.Contains(strings.ToLower(reason), "cancelled") {
					failures = append(failures, failedEvent{
						logicalID: unptr(evt.LogicalResourceId),
						resType:   unptr(evt.ResourceType),
						status:    evt.ResourceStatus,
						reason:    reason,
						timestamp: unptr(evt.Timestamp),
					})
				}
			}
		}
		if terminal != "" {
			slices.Reverse(failures)
			return &updateFailed{stackARN: unptr(stack.StackId), status: terminal, failures: failures}
		}
	}
}

// updateFailed is the error returned when a stack update rolls back. It
// carries the failures seen during the update, oldest first: the first one is
// the likely root cause, later ones usually cascade from it.
type updateFailed struct {
	stackARN string
	status   types.ResourceStatus
	failures []failedEvent
}

// failedEvent is a stack event that reports a failure.
type failedEvent struct {
	logicalID string
	resType   string
	status    types.ResourceStatus
	reason    string
	timestamp time.Time
}

func (e *updateFailed) Error() string {
	if len(e.failures) == 0 {
		return fmt.Sprintf("%s, see stack events: %s", e.status, e.consoleURL())
	}
	root := e.failures[0]
	s := fmt.Sprintf("%s (%s) %s", root.logicalID, root.resType, root.status)
	if reason := oneLine(root.reason); reason != "" {
		s += ": " + reason
	}
	return s + ", see stack events: " + e.consoleURL()
}

// writeSummary writes a Markdown report of all failures to the named file,
// meant for the GitHub Actions job summary.
func (e *updateFailed) writeSummary(filename string) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "## ❌ CloudFormation deployment failed (%s)\n\n", e.status)
	if len(e.failures) != 0 {
		b.WriteString("| Time (UTC) | Resource | Type | Status | Reason |\n| --- | --- | --- | --- | --- |\n")
		for _, f := range e.failures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", f.timestamp.UTC().Format("15:04:05"), f.logicalID, f.resType, f.status, mdCell(f.reason))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "[View stack events in the AWS Console](%s)\n", e.consoleURL())
	return os.WriteFile(filename, b.Bytes(), 0666)
}

func (e *updateFailed) consoleURL() string {
	return "https://console.aws.amazon.com/go/view?arn=" + url.QueryEscape(e.stackARN)
}

// oneLine collapses runs of whitespace (including newlines) into single spaces
// so a reason renders on a single log line or table cell.
func oneLine(s string) string {
	var prevIsSpace bool
	f := func(r rune) rune {
		if !unicode.IsSpace(r) {
			prevIsSpace = false
			return r
		}
		if prevIsSpace {
			return -1
		}
		prevIsSpace = true
		return ' '
	}
	return strings.TrimSpace(strings.Map(f, s))
}

// mdCell makes a reason safe to embed in a Markdown table cell.
func mdCell(s string) string { return strings.ReplaceAll(oneLine(s), "|", "\\|") }

func newToken() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "ucs-" + hex.EncodeToString(b)
}

func debugf(format string, args ...any) {
	if !underGithub {
		return
	}
	log.Printf("::debug::"+format, args...)
}

func init() {
	const usage = `Updates CloudFormation stack by updating some of its parameters while preserving all other settings.

Usage: update-cloudformation-stack -stack=NAME Param1=Value1 [Param2=Value2 ...]
`
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
}

var underGithub bool
var githubWarnPrefix string
var githubErrPrefix string

func init() {
	underGithub = os.Getenv("GITHUB_ACTIONS") == "true"
	if underGithub {
		githubWarnPrefix = "::warning::"
		githubErrPrefix = "::error::"
	}
}

func parseKvs(list []string) (map[string]string, error) {
	out := make(map[string]string)
	for _, line := range list {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("wrong parameter format, want key=value pair: %q", line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			return nil, fmt.Errorf("wrong parameter format, key must be non-empty: %q", line)
		}
		if _, ok := out[k]; ok {
			return nil, fmt.Errorf("duplicate key in parameters list: %q", k)
		}
		out[k] = v
	}
	return out, nil
}

func unptr[T any](v *T) T {
	var zero T
	if v != nil {
		return *v
	}
	return zero
}
