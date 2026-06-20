package main

import "testing"

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

func Test_hasTransform(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "yaml with transform", body: "AWSTemplateFormatVersion: '2010-09-09'\nTransform: AWS::Serverless-2016-10-31\nResources: {}\n", want: true},
		{name: "yaml transform list", body: "Transform:\n  - AWS::Serverless-2016-10-31\nResources: {}\n", want: true},
		{name: "yaml no transform", body: "AWSTemplateFormatVersion: '2010-09-09'\nResources:\n  Q:\n    Type: AWS::SQS::Queue\n", want: false},
		{name: "yaml nested transform key", body: "Resources:\n  Transform: something\n", want: false},
		{name: "json with transform", body: `{"Transform": "AWS::Serverless-2016-10-31", "Resources": {}}`, want: true},
		{name: "json transform list", body: `{"Transform": ["AWS::Serverless-2016-10-31"], "Resources": {}}`, want: true},
		{name: "json no transform", body: `{"Resources": {}}`, want: false},
		{name: "json null transform", body: `{"Transform": null, "Resources": {}}`, want: false},
		{name: "empty", body: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasTransform(tc.body); got != tc.want {
				t.Errorf("hasTransform() = %v, want %v", got, tc.want)
			}
		})
	}
}
