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
