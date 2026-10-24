package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

type commonOptions struct {
	logDirectory  string
	timeRange     string
	humanReadable bool
	priceFile     string
}

type proxyOptions struct {
	apiKey    string
	debugPath string
	listen    string
}

type requestOptions struct {
	ascending  bool
	descending bool
	num        int
}

type commandLine struct {
	command   string
	common    commonOptions
	proxy     proxyOptions
	request   requestOptions
	authFiles []string
}

func registerCommonFlags(flags *flag.FlagSet, options *commonOptions) {
	flags.StringVar(&options.logDirectory, "log", options.logDirectory, "directory for SQLite request usage logging and queries")
	flags.StringVar(&options.timeRange, "time", options.timeRange, "time range: START,END or 2m/1h/2d/1w/3M/1y; m: minutes, M: calendar months")
	flags.BoolVar(&options.humanReadable, "human", options.humanReadable, "format numeric output with compact K/M/B units")
	flags.StringVar(&options.priceFile, "price", options.priceFile, "JSON price table for request cost estimates")
}

func parseCommandLine(arguments []string, output io.Writer) (commandLine, error) {
	var options commandLine
	commonFlags := flag.NewFlagSet("common", flag.ContinueOnError)
	registerCommonFlags(commonFlags, &options.common)
	proxyFlags := flag.NewFlagSet("proxy", flag.ContinueOnError)
	proxyFlags.StringVar(&options.proxy.apiKey, "key", "", "required client Bearer API key")
	proxyFlags.StringVar(&options.proxy.debugPath, "debug", "", "optional JSON Lines debug log path")
	proxyFlags.StringVar(&options.proxy.listen, "listen", "", "listen address (overrides CODEX_PROXY_LISTEN)")

	flags := flag.NewFlagSet("codex-pass", flag.ContinueOnError)
	flags.SetOutput(output)
	for _, group := range []*flag.FlagSet{commonFlags, proxyFlags} {
		group.SetOutput(output)
		group.VisitAll(func(option *flag.Flag) {
			flags.Var(option.Value, option.Name, option.Usage)
		})
	}
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: codex-pass [common options] <command>")
		fmt.Fprintln(output, "       codex-pass [common options] --key API_KEY [--listen ADDRESS] [--debug FILE] AUTH_FILE [AUTH_FILE ...]")
		fmt.Fprintln(output, "\nTransparent Codex Responses API proxy listening on 127.0.0.1:8317 by default.")
		fmt.Fprintln(output, "\nCommands:")
		fmt.Fprintln(output, "  sum  Print SQLite request usage summary (requires --log DIR)")
		fmt.Fprintln(output, "  req  Print individual request logs (requires --log DIR)")
		fmt.Fprintln(output, "\nCommon options (before or after a command):")
		commonFlags.PrintDefaults()
		fmt.Fprintln(output, "\nProxy options:")
		proxyFlags.PrintDefaults()
	}

	rootArguments, command, commandArguments := splitCommand(arguments, flags)
	if err := flags.Parse(rootArguments); err != nil {
		return options, err
	}
	options.command = command
	if command == "" {
		options.authFiles = flags.Args()
		if strings.TrimSpace(options.proxy.apiKey) == "" {
			err := fmt.Errorf("--key is required")
			fmt.Fprintln(output, "error:", err)
			flags.Usage()
			return options, err
		}
		if len(options.authFiles) == 0 {
			err := fmt.Errorf("at least one Codex auth file is required")
			fmt.Fprintln(output, "error:", err)
			flags.Usage()
			return options, err
		}
		return options, nil
	}

	var proxyFlag string
	flags.Visit(func(option *flag.Flag) {
		if proxyFlag == "" && proxyFlags.Lookup(option.Name) != nil {
			proxyFlag = option.Name
		}
	})
	if proxyFlag != "" {
		err := fmt.Errorf("--%s is only available in proxy mode", proxyFlag)
		fmt.Fprintln(output, "error:", err)
		return options, err
	}

	commandFlags := flag.NewFlagSet("codex-pass "+command, flag.ContinueOnError)
	commandFlags.SetOutput(output)
	registerCommonFlags(commandFlags, &options.common)
	requestFlags := flag.NewFlagSet("req", flag.ContinueOnError)
	requestFlags.SetOutput(output)
	if command == "req" {
		requestFlags.BoolVar(&options.request.ascending, "asc", false, "sort requests by start time, oldest first")
		requestFlags.BoolVar(&options.request.descending, "desc", false, "sort requests by start time, newest first (default order)")
		requestFlags.IntVar(&options.request.num, "num", 0, "maximum number of request records (positive integer; default: no limit)")
		requestFlags.VisitAll(func(option *flag.Flag) {
			commandFlags.Var(option.Value, option.Name, option.Usage)
		})
	}
	commandFlags.Usage = func() {
		fmt.Fprintf(output, "Usage: codex-pass [common options] %s --log DIR [--time RANGE] [--human] [--price FILE]", command)
		if command == "req" {
			fmt.Fprintln(output, " [--asc | --desc] [--num N]")
			fmt.Fprintln(output, "\nPrint request counts and individual request logs, newest first by default.")
		} else {
			fmt.Fprintln(output)
			fmt.Fprintln(output, "\nPrint SQLite request usage summary.")
		}
		fmt.Fprintln(output, "\nCommon options (before or after the command):")
		commonFlags.PrintDefaults()
		if command == "req" {
			fmt.Fprintln(output, "\nRequest options (after req):")
			requestFlags.PrintDefaults()
		}
	}
	if err := commandFlags.Parse(commandArguments); err != nil {
		return options, err
	}
	if commandFlags.NArg() != 0 {
		err := fmt.Errorf("%s does not accept positional arguments", command)
		fmt.Fprintln(output, "error:", err)
		return options, err
	}
	if options.request.ascending && options.request.descending {
		err := fmt.Errorf("--asc and --desc cannot be used together")
		fmt.Fprintln(output, "error:", err)
		return options, err
	}
	var numSpecified bool
	commandFlags.Visit(func(option *flag.Flag) {
		if option.Name == "num" {
			numSpecified = true
		}
	})
	if numSpecified && options.request.num <= 0 {
		err := fmt.Errorf("--num must be a positive integer")
		fmt.Fprintln(output, "error:", err)
		return options, err
	}
	if strings.TrimSpace(options.common.logDirectory) == "" {
		err := fmt.Errorf("%s requires --log DIR", command)
		fmt.Fprintln(output, "error:", err)
		commandFlags.Usage()
		return options, err
	}
	return options, nil
}

// Only the first positional argument can select a command. Flag values and
// arguments after -- keep their usual meaning, even when named after a command.
func splitCommand(arguments []string, flags *flag.FlagSet) ([]string, string, []string) {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			switch argument {
			case "sum", "req":
				return arguments[:index], argument, arguments[index+1:]
			}
			break
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-"), "=")
		option := flags.Lookup(name)
		if option == nil {
			break // Let flag.Parse report unknown flags or handle --help.
		}
		boolean, isBoolean := option.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && (!isBoolean || !boolean.IsBoolFlag()) {
			index++
		}
	}
	return arguments, "", nil
}
