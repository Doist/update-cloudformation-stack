package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

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
		// Re-scan events on every tick. The stack-level terminal event is the
		// newest one, but the resource failures that caused it are older, so on
		// failure we keep scanning the page to collect them all before reporting.
		failures := make(map[string]failedEvent)
		var terminal types.ResourceStatus
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
						types.ResourceStatusRollbackFailed:
						terminal = evt.ResourceStatus
					}
					continue
				}
				if isFailure(evt.ResourceStatus, unptr(evt.ResourceStatusReason)) {
					fe := failedEvent{
						logicalID: unptr(evt.LogicalResourceId),
						resType:   unptr(evt.ResourceType),
						status:    evt.ResourceStatus,
						reason:    unptr(evt.ResourceStatusReason),
						timestamp: unptr(evt.Timestamp),
					}
					failures[fe.logicalID+"\x00"+fe.timestamp.String()] = fe
				}
			}
		}
		if terminal != "" {
			return reportFailure(cfg.Region, unptr(stack.StackId), terminal, sortedFailures(failures))
		}
	}
}

// failedEvent is a resource-level stack event that reports a failure.
type failedEvent struct {
	logicalID string
	resType   string
	status    types.ResourceStatus
	reason    string
	timestamp time.Time
}

// isFailure reports whether a resource-level event is a genuine failure, as
// opposed to a cancellation cascading from another resource's failure.
func isFailure(status types.ResourceStatus, reason string) bool {
	if !strings.HasSuffix(string(status), "_FAILED") {
		return false
	}
	return !strings.Contains(strings.ToLower(reason), "cancelled")
}

func sortedFailures(m map[string]failedEvent) []failedEvent {
	out := slices.Collect(maps.Values(m))
	slices.SortFunc(out, func(a, b failedEvent) int { return a.timestamp.Compare(b.timestamp) })
	return out
}

// reportFailure writes a human-readable failure report — a GitHub step summary
// table plus per-resource error annotations — and returns the most likely root
// cause as an error. The earliest failure is the root cause; later ones usually
// cascade from it.
func reportFailure(region, stackID string, terminal types.ResourceStatus, failures []failedEvent) error {
	consoleURL := eventsConsoleURL(region, stackID)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if err := writeStepSummary(path, consoleURL, terminal, failures); err != nil {
			debugf("cannot write step summary: %v", err)
		}
	}
	if len(failures) == 0 {
		return fmt.Errorf("%s, see stack events: %s", terminal, consoleURL)
	}
	for _, e := range failures[1:] {
		log.Printf("%s%s (%s) %s: %s", githubErrPrefix, e.logicalID, e.resType, e.status, oneLine(e.reason))
	}
	root := failures[0]
	return fmt.Errorf("%s (%s) %s: %s — see stack events: %s", root.logicalID, root.resType, root.status, oneLine(root.reason), consoleURL)
}

func eventsConsoleURL(region, stackID string) string {
	return fmt.Sprintf("https://%[1]s.console.aws.amazon.com/cloudformation/home?region=%[1]s#/stacks/events?stackId=%s",
		region, url.QueryEscape(stackID))
}

func writeStepSummary(path, consoleURL string, terminal types.ResourceStatus, failures []failedEvent) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "## ❌ CloudFormation deployment failed (%s)\n\n", terminal)
	if len(failures) != 0 {
		b.WriteString("| Time (UTC) | Resource | Type | Status | Reason |\n| --- | --- | --- | --- | --- |\n")
		for _, e := range failures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", e.timestamp.UTC().Format("15:04:05"), e.logicalID, e.resType, e.status, mdCell(e.reason))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "[View stack events in the AWS Console](%s)\n", consoleURL)
	_, err = io.WriteString(f, b.String())
	return err
}

// oneLine collapses runs of whitespace (including newlines) into single spaces
// so a reason renders on a single log line or table cell.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

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
