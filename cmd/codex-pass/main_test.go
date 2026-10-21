package main

import "testing"

func TestResolveListenAddress(t *testing.T) {
	tests := []struct {
		name string
		cli  string
		env  string
		want string
	}{
		{name: "default", want: "127.0.0.1:8317"},
		{name: "environment", env: "127.0.0.1:9000", want: "127.0.0.1:9000"},
		{name: "cli overrides environment", cli: "0.0.0.0:9000", env: "127.0.0.1:9000", want: "0.0.0.0:9000"},
		{name: "whitespace is unset", cli: "  ", env: "127.0.0.1:9000", want: "127.0.0.1:9000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveListenAddress(test.cli, test.env); got != test.want {
				t.Fatalf("resolveListenAddress(%q, %q) = %q, want %q", test.cli, test.env, got, test.want)
			}
		})
	}
}
