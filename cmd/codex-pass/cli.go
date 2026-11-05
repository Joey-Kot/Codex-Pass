package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"codex-pass/internal/codexapi"
)

const (
	sqliteFlag  = "codexpass-sqlite"
	debugFlag   = "codexpass-debug"
	sqliteEnv   = "CODEXPASS_SQLITE"
	debugEnv    = "CODEXPASS_DEBUG"
	authFileEnv = "CODEXPASS_AUTH_FILE"
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
	codex     bool
}

type requestOptions struct {
	ascending  bool
	descending bool
	num        int
	models     []string
}

type accountOptions struct {
	resetNumber   int
	clientVersion string
}

type commandLine struct {
	command   string
	common    commonOptions
	proxy     proxyOptions
	request   requestOptions
	account   accountOptions
	authFiles []string
}

func registerCommonFlags(flags *flag.FlagSet, options *commonOptions) {
	flags.StringVar(&options.logDirectory, sqliteFlag, options.logDirectory, "SQLite usage log `DIR` (defaults to "+sqliteEnv+")")
	flags.StringVar(&options.timeRange, "time", options.timeRange, "time `RANGE` as START,END or 2m/1h/2d/1w/3M/1y (m for minutes, M for months)")
	flags.BoolVar(&options.humanReadable, "human", options.humanReadable, "format numeric output with compact K/M/B units")
	flags.StringVar(&options.priceFile, "price", options.priceFile, "JSON `FILE` with prices for request cost estimates")
}

func parseCommandLine(arguments []string, output io.Writer) (commandLine, error) {
	var options commandLine
	commonFlags := flag.NewFlagSet("common", flag.ContinueOnError)
	registerCommonFlags(commonFlags, &options.common)
	proxyFlags := flag.NewFlagSet("proxy", flag.ContinueOnError)
	proxyFlags.StringVar(&options.proxy.apiKey, "key", "", "required client Bearer `KEY`")
	proxyFlags.StringVar(&options.proxy.debugPath, debugFlag, "", "JSON Lines debug log `FILE` (defaults to "+debugEnv+")")
	proxyFlags.StringVar(&options.proxy.listen, "listen", "", "listen `ADDRESS` (overrides CODEX_PROXY_LISTEN)")
	proxyFlags.BoolVar(&options.proxy.codex, "codex", false, "enable /codex for Codex with ChatGPT login (no local API key check; raw request bodies)")

	flags := flag.NewFlagSet("codex-pass", flag.ContinueOnError)
	flags.SetOutput(output)
	for _, group := range []*flag.FlagSet{commonFlags, proxyFlags} {
		group.VisitAll(func(option *flag.Flag) {
			flags.Var(option.Value, option.Name, option.Usage)
		})
	}
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: codex-pass [common options] <command> [command arguments]")
		fmt.Fprintln(output, "       codex-pass [common options] --key API_KEY [--codex] [--listen ADDRESS] [--codexpass-debug FILE] [AUTH_FILE ...]")
		fmt.Fprintln(output, "\nTransparent Codex Responses API proxy listening on 127.0.0.1:8317 by default.")
		fmt.Fprintln(output, "\nCommands:")
		fmt.Fprintln(output, "  sum  Print SQLite request usage summary")
		fmt.Fprintln(output, "  req  Print individual request logs")
		fmt.Fprintln(output, "  quota [AUTH_FILE ...]       Show weekly Codex quota usage")
		fmt.Fprintln(output, "  reset [N] [AUTH_FILE ...]   List usage limit resets or redeem reset N")
		fmt.Fprintln(output, "  models [--client-version VERSION] [AUTH_FILE ...]   List account model catalogs")
		fmt.Fprintln(output)
		printAuthFileHelp(output)
		fmt.Fprintln(output, "\nCommon options (before or after sum or req):")
		printLongFlags(output, commonFlags)
		fmt.Fprintln(output, "\nProxy options:")
		printLongFlags(output, proxyFlags)
	}

	operands, terminated, err := parseLongFlags(flags, arguments)
	if err != nil {
		return options, err
	}
	if !terminated && len(operands) > 0 {
		switch operands[0] {
		case "sum", "req", "quota", "reset", "models":
			options.command, operands = operands[0], operands[1:]
		}
	}
	command := options.command
	if command == "" {
		options.authFiles = resolveAuthFiles(operands)
		if strings.TrimSpace(options.proxy.apiKey) == "" {
			err := fmt.Errorf("--key is required")
			fmt.Fprintln(output, "error:", err)
			flags.Usage()
			return options, err
		}
		if len(options.authFiles) == 0 {
			err := fmt.Errorf("at least one Codex auth file is required (pass paths or set %s)", authFileEnv)
			fmt.Fprintln(output, "error:", err)
			flags.Usage()
			return options, err
		}
		if !hasFlag(flags, sqliteFlag) {
			options.common.logDirectory = os.Getenv(sqliteEnv)
		}
		if !hasFlag(flags, debugFlag) {
			options.proxy.debugPath = os.Getenv(debugEnv)
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

	switch command {
	case "quota", "reset", "models":
		var reportFlag string
		flags.Visit(func(option *flag.Flag) {
			if reportFlag == "" {
				reportFlag = option.Name
			}
		})
		if reportFlag != "" {
			err := fmt.Errorf("--%s is unavailable with %s", reportFlag, command)
			fmt.Fprintln(output, "error:", err)
			return options, err
		}
		err := parseAccountOptions(&options, operands, output)
		return options, err
	}

	commandFlags := flag.NewFlagSet("codex-pass "+command, flag.ContinueOnError)
	commandFlags.SetOutput(output)
	registerCommonFlags(commandFlags, &options.common)
	requestFlags := flag.NewFlagSet("req", flag.ContinueOnError)
	if command == "req" {
		requestFlags.BoolVar(&options.request.ascending, "asc", false, "sort requests by start time, oldest first")
		requestFlags.BoolVar(&options.request.descending, "desc", false, "sort requests by start time, newest first (default order)")
		requestFlags.IntVar(&options.request.num, "num", 0, "maximum `N` request records (positive integer, unlimited by default)")
		requestFlags.Func("models", "filter by exact `MODEL[,MODEL...]` names", func(value string) error {
			models := strings.Split(value, ",")
			for index, model := range models {
				models[index] = strings.TrimSpace(model)
				if models[index] == "" {
					return fmt.Errorf("model names must not be empty")
				}
			}
			options.request.models = models
			return nil
		})
		requestFlags.VisitAll(func(option *flag.Flag) {
			commandFlags.Var(option.Value, option.Name, option.Usage)
		})
	}
	commandFlags.Usage = func() {
		fmt.Fprintf(output, "Usage: codex-pass [common options] %s [--codexpass-sqlite DIR] [--time RANGE] [--human] [--price FILE]", command)
		if command == "req" {
			fmt.Fprintln(output, " [--asc | --desc] [--num N] [--models MODEL[,MODEL...]]")
			fmt.Fprintln(output, "\nPrint request counts and individual request logs, newest first by default.")
		} else {
			fmt.Fprintln(output)
			fmt.Fprintln(output, "\nPrint SQLite request usage summary.")
		}
		fmt.Fprintln(output, "\nCommon options (before or after the command):")
		printLongFlags(output, commonFlags)
		if command == "req" {
			fmt.Fprintln(output, "\nRequest options (after req):")
			printLongFlags(output, requestFlags)
		}
	}
	operands, _, err = parseLongFlags(commandFlags, operands)
	if err != nil {
		return options, err
	}
	if len(operands) != 0 {
		err := fmt.Errorf("%s does not accept positional arguments", command)
		fmt.Fprintln(output, "error:", err)
		return options, err
	}
	if options.request.ascending && options.request.descending {
		err := fmt.Errorf("--asc and --desc cannot be used together")
		fmt.Fprintln(output, "error:", err)
		return options, err
	}
	if hasFlag(commandFlags, "num") && options.request.num <= 0 {
		err := fmt.Errorf("--num must be a positive integer")
		fmt.Fprintln(output, "error:", err)
		return options, err
	}
	if !hasFlag(flags, sqliteFlag) && !hasFlag(commandFlags, sqliteFlag) {
		options.common.logDirectory = os.Getenv(sqliteEnv)
	}
	if strings.TrimSpace(options.common.logDirectory) == "" {
		err := fmt.Errorf("%s requires --%s DIR or %s", command, sqliteFlag, sqliteEnv)
		fmt.Fprintln(output, "error:", err)
		commandFlags.Usage()
		return options, err
	}
	return options, nil
}

func parseAccountOptions(options *commandLine, arguments []string, output io.Writer) error {
	command := options.command
	flags := flag.NewFlagSet("codex-pass "+command, flag.ContinueOnError)
	flags.SetOutput(output)
	usage := command
	switch command {
	case "reset":
		usage += " [N]"
	case "models":
		usage += " [--client-version VERSION]"
		flags.StringVar(&options.account.clientVersion, "client-version", codexapi.ModelsClientVersion, "Codex model catalog client `VERSION`")
	}
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: codex-pass "+usage+" [AUTH_FILE ...]")
		if command == "reset" {
			fmt.Fprintln(output, "List usage limit resets, or redeem the available reset numbered N (starting at 1) after confirmation.")
		}
		printAuthFileHelp(output)
		printLongFlags(output, flags)
	}
	paths, _, err := parseLongFlags(flags, arguments)
	if err != nil {
		return err
	}
	if command == "reset" && len(paths) > 0 && isResetNumber(paths[0]) {
		number, err := strconv.Atoi(paths[0])
		if err != nil || number < 1 {
			err := fmt.Errorf("reset number must be a positive integer")
			fmt.Fprintln(output, "error:", err)
			return err
		}
		options.account.resetNumber = number
		paths = paths[1:]
	}
	options.authFiles = resolveAuthFiles(paths)
	if len(options.authFiles) == 0 {
		err := fmt.Errorf("%s requires at least one Codex auth file (pass paths or set %s)", command, authFileEnv)
		fmt.Fprintln(output, "error:", err)
		flags.Usage()
		return err
	}
	if command == "models" && options.account.clientVersion == "" {
		options.account.clientVersion = codexapi.ModelsClientVersion
	}
	return nil
}

func isResetNumber(argument string) bool {
	if strings.HasPrefix(argument, "+") || strings.HasPrefix(argument, "-") {
		argument = argument[1:]
	}
	return argument != "" && strings.Trim(argument, "0123456789") == ""
}

func resolveAuthFiles(paths []string) []string {
	if len(paths) > 0 {
		return paths
	}
	return slices.DeleteFunc(filepath.SplitList(os.Getenv(authFileEnv)), func(path string) bool {
		return strings.TrimSpace(path) == ""
	})
}

func printAuthFileHelp(output io.Writer) {
	fmt.Fprintf(output, "%s supplies default auth paths separated by %q. Positional paths override it.\n", authFileEnv, string(os.PathListSeparator))
}

func hasFlag(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(option *flag.Flag) { set = set || option.Name == name })
	return set
}

func printLongFlags(output io.Writer, flags *flag.FlagSet) {
	flags.VisitAll(func(option *flag.Flag) {
		parameter, description := flag.UnquoteUsage(option)
		name := "--" + option.Name
		if parameter != "" {
			name += " " + parameter
		}
		if option.DefValue != "" && option.DefValue != "false" && option.DefValue != "0" {
			description += fmt.Sprintf(" (default %q)", option.DefValue)
		}
		fmt.Fprintf(output, "  %-26s %s\n", name, description)
	})
}

// FlagSet keeps the typed values and records which options were set. Parse only
// the leading GNU long options so the remaining arguments can select a command.
func parseLongFlags(flags *flag.FlagSet, arguments []string) (operands []string, terminated bool, err error) {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--":
			return arguments[index+1:], true, nil
		case argument == "-" || !strings.HasPrefix(argument, "-"):
			return arguments[index:], false, nil
		case argument == "--help":
			flags.Usage()
			return nil, false, flag.ErrHelp
		case !strings.HasPrefix(argument, "--"):
			return nil, false, printOptionError(flags, fmt.Errorf("invalid option %q (use -- for long options)", argument))
		}
		name, value, hasValue := strings.Cut(argument[2:], "=")
		option := flags.Lookup(name)
		if option == nil {
			return nil, false, printOptionError(flags, fmt.Errorf("unknown option --%s", name))
		}
		if !hasValue {
			if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
				value = "true"
			} else {
				index++
				if index == len(arguments) {
					return nil, false, printOptionError(flags, fmt.Errorf("option --%s requires a value", name))
				}
				value = arguments[index]
			}
		}
		if err := flags.Set(name, value); err != nil {
			return nil, false, printOptionError(flags, fmt.Errorf("invalid value %q for --%s: %w", value, name, err))
		}
	}
	return nil, false, nil
}

func printOptionError(flags *flag.FlagSet, err error) error {
	fmt.Fprintln(flags.Output(), "error:", err)
	flags.Usage()
	return err
}
