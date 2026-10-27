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
			arguments: []string{"sum", "--log", "usage", "--time", "1h", "--human"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1h", humanReadable: true},
		},
		{
			name:      "before command",
			arguments: []string{"--log", "usage", "--time", "2m", "--human", "sum"},
			want:      commonOptions{logDirectory: "usage", timeRange: "2m", humanReadable: true},
		},
		{
			name:      "both sides of command",
			arguments: []string{"--log", "usage", "sum", "--time", "1M", "--human"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1M", humanReadable: true},
		},
		{
			name:      "equals syntax",
			arguments: []string{"--log=usage", "--human=true", "sum", "--time=1h"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1h", humanReadable: true},
		},
		{
			name:      "later options override earlier values",
			arguments: []string{"--log", "old", "--time", "2m", "--human", "sum", "--log", "usage", "--time", "1h", "--human=false"},
			want:      commonOptions{logDirectory: "usage", timeRange: "1h"},
		},
		{
			name:      "command name as option value",
			arguments: []string{"--log", "sum", "sum"},
			want:      commonOptions{logDirectory: "sum"},
		},
		{
			name:      "terminator as option value",
			arguments: []string{"--log", "--", "sum"},
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
			if got.command != "sum" || got.common != test.want || got.proxy != (proxyOptions{}) || len(got.authFiles) != 0 {
				t.Fatalf("parsed options = %#v, want sum with %#v", got, test.want)
			}
		})
	}
}

func TestProxyCommandLine(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      commandLine
	}{
		{
			name:      "proxy and common options",
			arguments: []string{"--key", "test-key", "--log", "usage", "--time", "1h", "--human", "--debug", "proxy.jsonl", "--listen", "127.0.0.1:9000", "auth.json", "second.json"},
			want: commandLine{
				common:    commonOptions{logDirectory: "usage", timeRange: "1h", humanReadable: true},
				proxy:     proxyOptions{apiKey: "test-key", debugPath: "proxy.jsonl", listen: "127.0.0.1:9000"},
				authFiles: []string{"auth.json", "second.json"},
			},
		},
		{
			name:      "Codex entry alongside keyed API",
			arguments: []string{"--codex", "--key", "test-key", "auth.json"},
			want: commandLine{
				proxy:     proxyOptions{apiKey: "test-key", codex: true},
				authFiles: []string{"auth.json"},
			},
		},
		{
			name:      "command name as key and log directory",
			arguments: []string{"--key", "sum", "--log", "sum", "auth.json"},
			want: commandLine{
				common:    commonOptions{logDirectory: "sum"},
				proxy:     proxyOptions{apiKey: "sum"},
				authFiles: []string{"auth.json"},
			},
		},
		{
			name:      "command name after another auth file",
			arguments: []string{"--key", "test-key", "auth.json", "sum"},
			want: commandLine{
				proxy:     proxyOptions{apiKey: "test-key"},
				authFiles: []string{"auth.json", "sum"},
			},
		},
		{
			name:      "command name after terminator",
			arguments: []string{"--key", "test-key", "--", "sum"},
			want: commandLine{
				proxy:     proxyOptions{apiKey: "test-key"},
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

func TestReqCommandOptions(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      requestOptions
	}{
		{name: "default order", arguments: []string{"req", "--log", "usage"}},
		{name: "ascending", arguments: []string{"req", "--log", "usage", "--asc"}, want: requestOptions{ascending: true}},
		{name: "descending", arguments: []string{"req", "--log", "usage", "--desc"}, want: requestOptions{descending: true}},
		{name: "common options on both sides", arguments: []string{"--log", "usage", "--time", "1h", "req", "--human", "--asc"}, want: requestOptions{ascending: true}},
		{name: "disabled ascending", arguments: []string{"req", "--log", "usage", "--asc=false", "--desc"}, want: requestOptions{descending: true}},
		{name: "number only", arguments: []string{"req", "--log", "usage", "--num", "20"}, want: requestOptions{num: 20}},
		{name: "number with time and order", arguments: []string{"--time", "2m", "req", "--log", "usage", "--num=5", "--asc"}, want: requestOptions{ascending: true, num: 5}},
		{name: "later number overrides", arguments: []string{"req", "--log", "usage", "--num", "10", "--num", "3"}, want: requestOptions{num: 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseCommandLine(test.arguments, &output)
			if err != nil {
				t.Fatalf("parseCommandLine(%q): %v\n%s", test.arguments, err, output.String())
			}
			if got.command != "req" || got.common.logDirectory != "usage" || got.request != test.want {
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
			{"--price", "prices.json", command, "--log", "usage"},
			{command, "--log", "usage", "--price", "prices.json"},
			{"--price=old.json", "--log", "usage", command, "--price=prices.json"},
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
	tests := []struct {
		name      string
		arguments []string
		wantError string
	}{
		{name: "missing log", arguments: []string{"sum"}, wantError: "sum requires --log DIR"},
		{name: "empty log", arguments: []string{"sum", "--log", " "}, wantError: "sum requires --log DIR"},
		{name: "extra auth file", arguments: []string{"sum", "--log", "usage", "auth.json"}, wantError: "sum does not accept positional arguments"},
		{name: "old summary flag", arguments: []string{"--summary", "--log", "usage"}, wantError: "flag provided but not defined: -summary"},
		{name: "old summary flag after command", arguments: []string{"sum", "--summary", "--log", "usage"}, wantError: "flag provided but not defined: -summary"},
		{name: "proxy flag before sum", arguments: []string{"--key", "test-key", "sum", "--log", "usage"}, wantError: "--key is only available in proxy mode"},
		{name: "proxy flag after sum", arguments: []string{"sum", "--key", "test-key", "--log", "usage"}, wantError: "flag provided but not defined: -key"},
		{name: "missing flag value before sum", arguments: []string{"--time"}, wantError: "flag needs an argument: -time"},
		{name: "missing flag value after sum", arguments: []string{"sum", "--log"}, wantError: "flag needs an argument: -log"},
		{name: "invalid boolean", arguments: []string{"sum", "--human=maybe"}, wantError: "invalid boolean value"},
		{name: "missing proxy key", arguments: []string{"auth.json"}, wantError: "--key is required"},
		{name: "Codex entry still requires proxy key", arguments: []string{"--codex", "auth.json"}, wantError: "--key is required"},
		{name: "Codex entry is proxy only", arguments: []string{"--codex", "sum", "--log", "usage"}, wantError: "--codex is only available in proxy mode"},
		{name: "missing proxy auth file", arguments: []string{"--key", "test-key"}, wantError: "at least one Codex auth file is required"},
		{name: "req missing log", arguments: []string{"req"}, wantError: "req requires --log DIR"},
		{name: "req extra argument", arguments: []string{"req", "--log", "usage", "auth.json"}, wantError: "req does not accept positional arguments"},
		{name: "conflicting order", arguments: []string{"req", "--log", "usage", "--asc", "--desc"}, wantError: "--asc and --desc cannot be used together"},
		{name: "sum has no ascending option", arguments: []string{"sum", "--log", "usage", "--asc"}, wantError: "flag provided but not defined: -asc"},
		{name: "sum has no descending option", arguments: []string{"sum", "--log", "usage", "--desc"}, wantError: "flag provided but not defined: -desc"},
		{name: "missing price filename", arguments: []string{"req", "--log", "usage", "--price"}, wantError: "flag needs an argument: -price"},
		{name: "zero request limit", arguments: []string{"req", "--log", "usage", "--num", "0"}, wantError: "--num must be a positive integer"},
		{name: "negative request limit", arguments: []string{"req", "--log", "usage", "--num", "-1"}, wantError: "--num must be a positive integer"},
		{name: "fractional request limit", arguments: []string{"req", "--log", "usage", "--num", "1.5"}, wantError: "invalid value"},
		{name: "invalid request limit", arguments: []string{"req", "--log", "usage", "--num", "many"}, wantError: "invalid value"},
		{name: "overflowing request limit", arguments: []string{"req", "--log", "usage", "--num", "99999999999999999999999"}, wantError: "invalid value"},
		{name: "missing request limit", arguments: []string{"req", "--log", "usage", "--num"}, wantError: "flag needs an argument: -num"},
		{name: "sum has no number option", arguments: []string{"sum", "--log", "usage", "--num", "5"}, wantError: "flag provided but not defined: -num"},
		{name: "number is not a common option", arguments: []string{"--num", "5", "req", "--log", "usage"}, wantError: "flag provided but not defined: -num"},
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
	for _, arguments := range [][]string{{"--help"}, {"sum", "--help"}, {"--human", "sum", "--help"}, {"req", "--help"}} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseCommandLine(arguments, &output)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("help error = %v, want flag.ErrHelp", err)
			}
			for _, value := range []string{"Common options", "-log", "-time", "-human", "-price"} {
				if !strings.Contains(output.String(), value) {
					t.Errorf("help does not contain %q:\n%s", value, output.String())
				}
			}
			if strings.Contains(output.String(), "-summary") {
				t.Errorf("help still advertises the removed summary flag:\n%s", output.String())
			}
			if strings.Contains(strings.Join(arguments, " "), "sum") && strings.Contains(output.String(), "-key") {
				t.Errorf("sum help contains proxy options:\n%s", output.String())
			}
			if arguments[0] == "req" {
				for _, value := range []string{"req", "Request options", "-asc", "-desc", "-num"} {
					if !strings.Contains(output.String(), value) {
						t.Errorf("req help does not contain %q:\n%s", value, output.String())
					}
				}
				if strings.Contains(output.String(), "-key") {
					t.Errorf("req help contains proxy options:\n%s", output.String())
				}
			}
		})
	}
}
