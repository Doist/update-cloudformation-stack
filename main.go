package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/smithy-go"
)

func main() {
	log.SetFlags(0)
	var stackName string
	flag.StringVar(&stackName, "stack", stackName, "name of the CloudFormation stack to update")
	flag.Parse()
	if err := run(context.Background(), stackName, flag.Args()); err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "ValidationError" && ae.ErrorMessage() == "No updates are to be performed." {
			debugf("error: %v", err)
			log.Print(githubWarnPrefix, "nothing to update")
			return
		}
		log.Fatal(githubErrPrefix, err)
	}
}

var (
	defaultPollInterval = 15 * time.Second
	defaultPollTimeout  = 10 * time.Minute
)

func run(ctx context.Context, stackName string, args []string) error {
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

	// Ensure the stack is stable before commencing update.
	stack, err := waitForStableStack(ctx, svc, stackName, defaultPollInterval, defaultPollTimeout)
	if err != nil {
		return fmt.Errorf("waiting for stable stack: %w", err)
	}
	log.Printf("stack %s is ready, status: %s", stackName, stack.StackStatus)

	var params []types.Parameter
	for _, p := range stack.Parameters {
		k := aws.ToString(p.ParameterKey)
		if v, ok := toReplace[k]; ok {
			params = append(params, types.Parameter{ParameterKey: &k, ParameterValue: &v})
			delete(toReplace, k)
			continue
		}
		params = append(params, types.Parameter{ParameterKey: &k, UsePreviousValue: aws.Bool(true)})
	}
	if len(toReplace) != 0 {
		return fmt.Errorf("stack has no parameters with these names: %s", strings.Join(slices.Sorted(maps.Keys(toReplace)), ", "))
	}

	debugf("parameters to call UpdateStack with:")
	for _, p := range params {
		switch {
		case aws.ToBool(p.UsePreviousValue):
			debugf("%s (use the previous value)", aws.ToString(p.ParameterKey))
		default:
			debugf("%s: %s", aws.ToString(p.ParameterKey), aws.ToString(p.ParameterValue))
		}
	}

	token := newToken()
	_, err = svc.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName:           &stackName,
		ClientRequestToken:  &token,
		UsePreviousTemplate: aws.Bool(true),
		Parameters:          params,
		Capabilities:        stack.Capabilities,
		NotificationARNs:    stack.NotificationARNs,
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
		p := cloudformation.NewDescribeStackEventsPaginator(svc, &cloudformation.DescribeStackEventsInput{StackName: &stackName})
	scanEvents:
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, evt := range page.StackEvents {
				if evt.Timestamp != nil && aws.ToTime(evt.Timestamp).Before(oldEventsCutoff) {
					break scanEvents
				}
				if evt.ClientRequestToken == nil || *evt.ClientRequestToken != token {
					continue
				}
				if evt.ResourceStatus == types.ResourceStatusUpdateFailed && aws.ToString(evt.ResourceStatusReason) != "Resource update cancelled" {
					return fmt.Errorf("%v: %s", evt.ResourceStatus, aws.ToString(evt.ResourceStatusReason))
				}
				debugf("%s\t%s\t%v", aws.ToString(evt.ResourceType), aws.ToString(evt.LogicalResourceId), evt.ResourceStatus)
				if aws.ToString(evt.LogicalResourceId) == stackName && aws.ToString(evt.ResourceType) == "AWS::CloudFormation::Stack" {
					switch evt.ResourceStatus {
					case types.ResourceStatusUpdateRollbackComplete,
						types.ResourceStatusRollbackFailed:
						return fmt.Errorf("%v, see AWS CloudFormation Console for more details", evt.ResourceStatus)
					case types.ResourceStatusUpdateComplete:
						return nil
					}
				}
			}
		}
	}
}

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
		if k == "" || v == "" {
			return nil, fmt.Errorf("wrong parameter format, both key and value must be non-empty: %q", line)
		}
		if _, ok := out[k]; ok {
			return nil, fmt.Errorf("duplicate key in parameters list: %q", k)
		}
		out[k] = v
	}
	return out, nil
}

type stackDescriber interface {
	DescribeStacks(ctx context.Context, input *cloudformation.DescribeStacksInput, optFns ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error)
}

func waitForStableStack(ctx context.Context, svc stackDescriber, stackName string, interval time.Duration, timeout time.Duration) (types.Stack, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
		case <-timer.C:
			return types.Stack{}, fmt.Errorf("timed out waiting for stack %s to reach a stable state", stackName)
		case <-ctx.Done():
			return types.Stack{}, ctx.Err()
		}

		desc, err := svc.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackName})
		if err != nil {
			return types.Stack{}, err
		}
		if len(desc.Stacks) != 1 {
			return types.Stack{}, fmt.Errorf("unexpected number of stacks returned: %d", len(desc.Stacks))
		}
		status := desc.Stacks[0].StackStatus
		log.Printf("current stack status: %s", status)

		switch status {
		case types.StackStatusCreateComplete,
			types.StackStatusUpdateComplete,
			types.StackStatusRollbackComplete,
			types.StackStatusUpdateRollbackComplete:
			// Stable states.
			log.Printf("stack %s is in a stable state: %s", stackName, status)
			stack := desc.Stacks[0]
			return stack, nil
		case types.StackStatusUpdateInProgress,
			types.StackStatusRollbackInProgress,
			types.StackStatusUpdateRollbackInProgress,
			types.StackStatusReviewInProgress:
			// In-progress, wait longer.
			log.Printf("stack %s is busy (state: %s), waiting...", stackName, status)
			continue
		default:
			// Uh oh.
			return types.Stack{}, fmt.Errorf("stack %s is in an unexpected or error state: %s", stackName, status)
		}
	}
}
