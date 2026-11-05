package main

import (
	"bytes"
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestSumCommonOptions(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      commonOptions
	}{
		{
			name:      "after command",
			arguments: []string{"sum", "--codexpass-sqlite", "usage", "--time", "1h", "--human"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1h", humanReadable: true},
		},
		{
			name:      "before command",
			arguments: []string{"--codexpass-sqlite", "usage", "--time", "2m", "--human", "sum"},
			want:      commonOptions{logDirectory: "usage", timeRange: "2m", humanReadable: true},
		},
		{
			name:      "both sides of command",
			arguments: []string{"--codexpass-sqlite", "usage", "sum", "--time", "1M", "--human"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1M", humanReadable: true},
		},
		{
			name:      "equals syntax",
			arguments: []string{"--codexpass-sqlite=usage", "--human=true", "sum", "--time=1h"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1h", humanReadable: true},
		},
		{
			name:      "later options override earlier values",
			arguments: []string{"--codexpass-sqlite", "old", "--time", "2m", "--human", "sum", "--codexpass-sqlite", "usage", "--time", "1h", "--human=false"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1h"},
		},
		{
			name:      "command name as option value",
			arguments: []string{"--codexpass-sqlite", "sum", "sum"},
			want:      commonOptions{logDirectory: "sum"},
		},
		{
			name:      "terminator as option value",
			arguments: []string{"--codexpass-sqlite", "--", "sum"},
			want:      commonOptions{logDirectory: "--"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseCommandLine(test.arguments, &output)
			if err != nil {
				t.Fatalf("parseCommandLine(%q): %v\n%s", test.arguments, err, output.String())
			}
			if got.command != "sum" || got.common != test.want || !reflect.DeepEqual(got.proxy, proxyOptions{}) || len(got.authFiles) != 0 {
				t.Fatalf("parsed options = %#v, want sum with %#v", got, test.want)
			}
		})
	}
}

func TestProxyCommandLine(t *testing.T) {
	t.Setenv("CODEXPASS_SQLITE", "")
	t.Setenv("CODEXPASS_DEBUG", "")
	tests := []struct {
		name      string
		arguments []string
		want      commandLine
	}{
		{
			name:      "proxy and common options",
			arguments: []string{"--key", "test-key", "--codexpass-sqlite", "usage", "--time", "1h", "--human", "--codexpass-debug", "proxy.jsonl", "--listen", "127.0.0.1:9000", "auth.json", "second.json"},
			want: commandLine{
				common:    commonOptions{logDirectory: "usage", timeRange: "1h", humanReadable: true},
				proxy:     proxyOptions{apiKeys: []string{"test-key"}, debugPath: "proxy.jsonl", listen: "127.0.0.1:9000"},
				authFiles: []string{"auth.json", "second.json"},
			},
		},
		{
			name:      "Codex entry alongside keyed API",
			arguments: []string{"--codex", "--key", "test-key", "auth.json"},
			want: commandLine{
				proxy:     proxyOptions{apiKeys: []string{"test-key"}, codex: true},
				authFiles: []string{"auth.json"},
			},
		},
		{
			name:      "command name as key and log directory",
			arguments: []string{"--key", "sum", "--codexpass-sqlite", "sum", "auth.json"},
			want: commandLine{
				common:    commonOptions{logDirectory: "sum"},
				proxy:     proxyOptions{apiKeys: []string{"sum"}},
				authFiles: []string{"auth.json"},
			},
		},
		{
			name:      "command name after another auth file",
			arguments: []string{"--key", "test-key", "auth.json", "sum"},
			want: commandLine{
				proxy:     proxyOptions{apiKeys: []string{"test-key"}},
				authFiles: []string{"auth.json", "sum"},
			},
		},
		{
			name:      "command name after terminator",
			arguments: []string{"--key", "test-key", "--", "sum"},
			want: commandLine{
				proxy:     proxyOptions{apiKeys: []string{"test-key"}},
				authFiles: []string{"sum"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseCommandLine(test.arguments, &output)
			if err != nil {
				t.Fatalf("parseCommandLine(%q): %v\n%s", test.arguments, err, output.String())
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parsed options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestProxyKeySources(t *testing.T) {
	for _, test := range []struct {
		name string
		env  string
		args []string
		want []string
	}{
		{name: "one environment key", env: "env-one", want: []string{"env-one"}},
		{name: "environment list", env: " env-one, env-two ", want: []string{"env-one", "env-two"}},
		{name: "command line overrides environment", env: "env-one,env-two", args: []string{"--key", "cli-one"}, want: []string{"cli-one"}},
		{name: "command line list overrides invalid environment", env: ",", args: []string{"--key=cli-one,cli-two"}, want: []string{"cli-one", "cli-two"}},
		{name: "explicit empty key", env: "env-one", args: []string{"--key="}},
		{name: "empty environment list entry", env: "example-private-key,,env-two"},
		{name: "empty command line list entry", env: "env-one", args: []string{"--key=example-private-key,"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CODEXPASS_KEY", test.env)
			var output bytes.Buffer
			options, err := parseCommandLine(append(test.args, "auth.json"), &output)
			if test.want == nil {
				if err == nil || !strings.Contains(output.String(), "non-empty API keys") || strings.Contains(output.String(), "example-private-key") {
					t.Fatalf("invalid key list produced error %v and output %q", err, output.String())
				}
				return
			}
			if err != nil || !reflect.DeepEqual(options.proxy.apiKeys, test.want) {
				t.Fatalf("parsed keys = %q, want %q, error = %v", options.proxy.apiKeys, test.want, err)
			}
		})
	}
}

func TestSQLiteAndDebugEnvironment(t *testing.T) {
	t.Setenv("CODEXPASS_SQLITE", "environment-directory")
	t.Setenv("CODEXPASS_DEBUG", "environment.jsonl")
	for _, test := range []struct {
		arguments []string
		directory string
		debug     string
	}{
		{[]string{"--key", "test-key", "auth.json"}, "environment-directory", "environment.jsonl"},
		{[]string{"--key", "test-key", "--codexpass-sqlite", "cli-directory", "--codexpass-debug=cli.jsonl", "auth.json"}, "cli-directory", "cli.jsonl"},
		{[]string{"--key", "test-key", "--codexpass-sqlite=", "--codexpass-debug=", "auth.json"}, "", ""},
		{[]string{"sum"}, "environment-directory", ""},
		{[]string{"req", "--models", "one,two"}, "environment-directory", ""},
		{[]string{"--codexpass-sqlite", "before-command", "sum"}, "before-command", ""},
		{[]string{"--codexpass-sqlite", "before-command", "req", "--codexpass-sqlite", "after-command"}, "after-command", ""},
		{[]string{"quota", "auth.json"}, "", ""},
	} {
		var output bytes.Buffer
		options, err := parseCommandLine(test.arguments, &output)
		if err != nil || options.common.logDirectory != test.directory || options.proxy.debugPath != test.debug {
			t.Errorf("parseCommandLine(%q) = SQLite %q, debug %q, error %v\n%s", test.arguments, options.common.logDirectory, options.proxy.debugPath, err, output.String())
		}
	}
	var output bytes.Buffer
	if _, err := parseCommandLine([]string{"req", "--codexpass-sqlite="}, &output); err == nil || !strings.Contains(output.String(), "requires --codexpass-sqlite") {
		t.Fatalf("explicit empty SQLite path fell back to the environment: %v\n%s", err, output.String())
	}
}

func TestReqCommandOptions(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      requestOptions
	}{
		{name: "default order", arguments: []string{"req", "--codexpass-sqlite", "usage"}},
		{name: "ascending", arguments: []string{"req", "--codexpass-sqlite", "usage", "--asc"}, want: requestOptions{ascending: true}},
		{name: "descending", arguments: []string{"req", "--codexpass-sqlite", "usage", "--desc"}, want: requestOptions{descending: true}},
		{name: "common options on both sides", arguments: []string{"--codexpass-sqlite", "usage", "--time", "1h", "req", "--human", "--asc"}, want: requestOptions{ascending: true}},
		{name: "disabled ascending", arguments: []string{"req", "--codexpass-sqlite", "usage", "--asc=false", "--desc"}, want: requestOptions{descending: true}},
		{name: "number only", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "20"}, want: requestOptions{num: 20}},
		{name: "number with time and order", arguments: []string{"--time", "2m", "req", "--codexpass-sqlite", "usage", "--num=5", "--asc"}, want: requestOptions{ascending: true, num: 5}},
		{name: "later number overrides", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "10", "--num", "3"}, want: requestOptions{num: 3}},
		{name: "one model", arguments: []string{"req", "--codexpass-sqlite", "usage", "--models", "gpt-6-sol"}, want: requestOptions{models: []string{"gpt-6-sol"}}},
		{name: "multiple models", arguments: []string{"req", "--codexpass-sqlite", "usage", "--models=gpt-6-sol, gpt-6-luna"}, want: requestOptions{models: []string{"gpt-6-sol", "gpt-6-luna"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseCommandLine(test.arguments, &output)
			if err != nil {
				t.Fatalf("parseCommandLine(%q): %v\n%s", test.arguments, err, output.String())
			}
			if got.command != "req" || got.common.logDirectory != "usage" || !reflect.DeepEqual(got.request, test.want) {
				t.Fatalf("parsed options = %#v, want req with %#v", got, test.want)
			}
			if test.name == "common options on both sides" && (got.common.timeRange != "1h" || !got.common.humanReadable) {
				t.Fatalf("common options = %#v", got.common)
			}
		})
	}
}

func TestPriceIsACommonOption(t *testing.T) {
	for _, command := range []string{"sum", "req"} {
		for _, arguments := range [][]string{
			{"--price", "prices.json", command, "--codexpass-sqlite", "usage"},
			{command, "--codexpass-sqlite", "usage", "--price", "prices.json"},
			{"--price=old.json", "--codexpass-sqlite", "usage", command, "--price=prices.json"},
		} {
			var output bytes.Buffer
			options, err := parseCommandLine(arguments, &output)
			if err != nil || options.command != command || options.common.priceFile != "prices.json" {
				t.Fatalf("parseCommandLine(%q) = %#v, error = %v: %s", arguments, options, err, output.String())
			}
		}
	}
	var output bytes.Buffer
	options, err := parseCommandLine([]string{"--key", "test-key", "--price", "req", "auth.json"}, &output)
	if err != nil || options.command != "" || options.common.priceFile != "req" {
		t.Fatalf("price filename was treated as a command: %#v, error = %v", options, err)
	}
}

func TestCommandLineRejectsInvalidArguments(t *testing.T) {
	t.Setenv("CODEXPASS_AUTH_FILE", "")
	t.Setenv("CODEXPASS_KEY", "")
	t.Setenv("AUTH_FILE", "legacy.json")
	t.Setenv("CODEXPASS_SQLITE", "")
	tests := []struct {
		name      string
		arguments []string
		wantError string
	}{
		{name: "missing log", arguments: []string{"sum"}, wantError: "sum requires --codexpass-sqlite DIR"},
		{name: "empty log", arguments: []string{"sum", "--codexpass-sqlite", " "}, wantError: "sum requires --codexpass-sqlite DIR"},
		{name: "extra auth file", arguments: []string{"sum", "--codexpass-sqlite", "usage", "auth.json"}, wantError: "sum does not accept positional arguments"},
		{name: "old summary flag", arguments: []string{"--summary", "--codexpass-sqlite", "usage"}, wantError: "unknown option --summary"},
		{name: "old summary flag after command", arguments: []string{"sum", "--summary", "--codexpass-sqlite", "usage"}, wantError: "unknown option --summary"},
		{name: "old SQLite flag", arguments: []string{"sum", "--log", "usage"}, wantError: "unknown option --log"},
		{name: "old debug flag", arguments: []string{"--debug", "debug.jsonl"}, wantError: "unknown option --debug"},
		{name: "single dash proxy option", arguments: []string{"-key", "test-key", "auth.json"}, wantError: "invalid option \"-key\""},
		{name: "single dash help", arguments: []string{"-h"}, wantError: "invalid option \"-h\""},
		{name: "single dash report option", arguments: []string{"req", "-time", "1h"}, wantError: "invalid option \"-time\""},
		{name: "single dash account option", arguments: []string{"models", "-client-version", "0.166.0"}, wantError: "invalid option \"-client-version\""},
		{name: "proxy flag before sum", arguments: []string{"--key", "test-key", "sum", "--codexpass-sqlite", "usage"}, wantError: "--key is only available in proxy mode"},
		{name: "proxy flag after sum", arguments: []string{"sum", "--key", "test-key", "--codexpass-sqlite", "usage"}, wantError: "unknown option --key"},
		{name: "missing flag value before sum", arguments: []string{"--time"}, wantError: "option --time requires a value"},
		{name: "missing flag value after sum", arguments: []string{"sum", "--codexpass-sqlite"}, wantError: "option --codexpass-sqlite requires a value"},
		{name: "invalid boolean", arguments: []string{"sum", "--human=maybe"}, wantError: "invalid value \"maybe\" for --human"},
		{name: "missing proxy key", arguments: []string{"auth.json"}, wantError: "--key or CODEXPASS_KEY"},
		{name: "Codex entry still requires proxy key", arguments: []string{"--codex", "auth.json"}, wantError: "--key or CODEXPASS_KEY"},
		{name: "Codex entry is proxy only", arguments: []string{"--codex", "sum", "--codexpass-sqlite", "usage"}, wantError: "--codex is only available in proxy mode"},
		{name: "missing proxy auth file", arguments: []string{"--key", "test-key"}, wantError: "at least one Codex auth file is required"},
		{name: "req missing log", arguments: []string{"req"}, wantError: "req requires --codexpass-sqlite DIR"},
		{name: "req extra argument", arguments: []string{"req", "--codexpass-sqlite", "usage", "auth.json"}, wantError: "req does not accept positional arguments"},
		{name: "conflicting order", arguments: []string{"req", "--codexpass-sqlite", "usage", "--asc", "--desc"}, wantError: "--asc and --desc cannot be used together"},
		{name: "sum has no ascending option", arguments: []string{"sum", "--codexpass-sqlite", "usage", "--asc"}, wantError: "unknown option --asc"},
		{name: "sum has no descending option", arguments: []string{"sum", "--codexpass-sqlite", "usage", "--desc"}, wantError: "unknown option --desc"},
		{name: "missing price filename", arguments: []string{"req", "--codexpass-sqlite", "usage", "--price"}, wantError: "option --price requires a value"},
		{name: "zero request limit", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "0"}, wantError: "--num must be a positive integer"},
		{name: "negative request limit", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "-1"}, wantError: "--num must be a positive integer"},
		{name: "fractional request limit", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "1.5"}, wantError: "invalid value"},
		{name: "invalid request limit", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "many"}, wantError: "invalid value"},
		{name: "overflowing request limit", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num", "99999999999999999999999"}, wantError: "invalid value"},
		{name: "missing request limit", arguments: []string{"req", "--codexpass-sqlite", "usage", "--num"}, wantError: "option --num requires a value"},
		{name: "sum has no number option", arguments: []string{"sum", "--codexpass-sqlite", "usage", "--num", "5"}, wantError: "unknown option --num"},
		{name: "number is not a common option", arguments: []string{"--num", "5", "req", "--codexpass-sqlite", "usage"}, wantError: "unknown option --num"},
		{name: "empty model list", arguments: []string{"req", "--models="}, wantError: "model names must not be empty"},
		{name: "empty model in list", arguments: []string{"req", "--models", "a,,b"}, wantError: "model names must not be empty"},
		{name: "sum has no model filter", arguments: []string{"sum", "--models", "a,b"}, wantError: "unknown option --models"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseCommandLine(test.arguments, &output)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
			if !strings.Contains(output.String(), test.wantError) {
				t.Fatalf("error output = %q, want %q", output.String(), test.wantError)
			}
		})
	}
}

func TestCommandLineHelp(t *testing.T) {
	for _, arguments := range [][]string{{"--help"}, {"sum", "--help"}, {"--human", "sum", "--help"}, {"req", "--help"}, {"models", "--help"}} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseCommandLine(arguments, &output)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("help error = %v, want flag.ErrHelp", err)
			}
			for _, line := range strings.Split(output.String(), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "--") {
					t.Errorf("help advertises a single-dash option: %q", line)
				}
			}
			if arguments[0] == "models" {
				for _, value := range []string{"--client-version", "CODEXPASS_AUTH_FILE"} {
					if !strings.Contains(output.String(), value) {
						t.Errorf("models help does not contain %q:\n%s", value, output.String())
					}
				}
				return
			}
			for _, value := range []string{"Common options", "--codexpass-sqlite", "CODEXPASS_SQLITE", "--time", "--human", "--price"} {
				if !strings.Contains(output.String(), value) {
					t.Errorf("help does not contain %q:\n%s", value, output.String())
				}
			}
			if strings.Contains(output.String(), "--summary") {
				t.Errorf("help still advertises the removed summary flag:\n%s", output.String())
			}
			if strings.Contains(strings.Join(arguments, " "), "sum") && strings.Contains(output.String(), "--key") {
				t.Errorf("sum help contains proxy options:\n%s", output.String())
			}
			if arguments[0] == "req" {
				for _, value := range []string{"req", "Request options", "--asc", "--desc", "--num", "--models"} {
					if !strings.Contains(output.String(), value) {
						t.Errorf("req help does not contain %q:\n%s", value, output.String())
					}
				}
				if strings.Contains(output.String(), "--key") {
					t.Errorf("req help contains proxy options:\n%s", output.String())
				}
			}
		})
	}
}
