# Update CloudFormation Stack Parameters Action

This GitHub Action updates existing CloudFormation stack by changing some of its parameters while preserving all other settings.

## Usage

```yaml
- uses: doist/update-cloudformation-stack@main
  with:
    stack: my-stack-name
    parameters: |
      Name1=value1
      Name2=value2
```

## Inputs

- `stack` - name of the CloudFormation stack to update
- `parameters` - pairs of parameters in the Name=Value format, each pair on a separate line
- `express` - optional, set to true to enable [CloudFormation express mode](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/cloudformation-express-mode.html)

## AWS Credentials

This action uses the AWS SDK default credential provider chain. Configure AWS credentials using standard GitHub Actions methods:

```yaml
- uses: aws-actions/configure-aws-credentials@v4
  with:
    role-to-assume: arn:aws:iam::123456789012:role/my-role
    aws-region: us-east-1
```

## AWS Permissions

This action requires the following permissions:

- cloudformation:DescribeStacks
- cloudformation:UpdateStack
- cloudformation:DescribeStackEvents

## Example

```yaml
jobs:
  update-stack:
    runs-on: ubuntu-latest
    steps:
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::123456789012:role/my-role
          aws-region: us-east-1
      - uses: doist/update-cloudformation-stack@main
        with:
          stack: production-stack
          parameters: |
            ImageTag=v123
```

The action will monitor stack update progress and fail if update fails.

## Failure reporting

When a stack update fails and rolls back, the action reports why instead of
leaving you to dig through the AWS Console:

- It scans the stack events for this specific update and reports the most likely
  root cause (the earliest failed resource) as the step's error message.
- Running under GitHub Actions, it writes a summary table of all failed
  resources (logical ID, type, status, reason) to the job summary, with a
  deep link to the stack's events in the AWS Console.

## Known Limitations

This action doesn't work for stacks that rely on template transformations
(stack template has non-empty [“Transform” section][transform]).

On such stacks UpdateStack API call does not recognize parameters-only changes and the action reports there's nothing to update.

[transform]: https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/transform-section-structure.html
