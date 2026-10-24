package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codex-pass/internal/auth"
	debuglog "codex-pass/internal/debug"
	"codex-pass/internal/proxy"
	"codex-pass/internal/requestlog"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	options, err := parseCommandLine(arguments, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch options.command {
	case "sum":
		return runSummaryWithOptions(options.common, os.Stdout, os.Stderr)
	case "req":
		return runRequestLogs(options.common, options.request, os.Stdout, os.Stderr)
	default:
		return runProxy(options.common, options.proxy, options.authFiles)
	}
}

func runProxy(common commonOptions, options proxyOptions, authFiles []string) int {
	credentials, err := auth.LoadAll(authFiles)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	httpClient := proxy.NewHTTPClient()
	pool, err := auth.NewPool(credentials, auth.RefreshConfig{Client: httpClient})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	var logger *debuglog.Logger
	if options.debugPath != "" {
		logger, err = debuglog.New(options.debugPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		defer func() {
			if closeErr := logger.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "debug log error:", closeErr)
			}
		}()
	}
	var usageLog *requestlog.Store
	if strings.TrimSpace(common.logDirectory) != "" {
		usageLog, err = requestlog.Open(common.logDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		defer func() {
			if closeErr := usageLog.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "request log error:", closeErr)
			}
		}()
	}
	handler, err := proxy.New(proxy.Config{
		APIKey: options.apiKey, AuthPool: pool, HTTPClient: httpClient, Logger: logger, RequestLog: usageLog,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	listenAddress := resolveListenAddress(options.listen, os.Getenv("CODEX_PROXY_LISTEN"))
	if !isLoopbackAddress(listenAddress) {
		fmt.Fprintln(os.Stderr, "warning: listen address is not loopback; put TLS, access control, and protected debug logs in front of this proxy")
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("codex-pass listening on http://%s with %d auth file(s)", listenAddress, len(credentials))
	if logger != nil {
		log.Printf("detailed debug logging enabled at %s", options.debugPath)
	}
	if usageLog != nil {
		log.Printf("request usage logging enabled at %s", usageLog.Path())
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-interrupts
		shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		return 1
	}
	return 0
}

func runSummary(logDirectory, timeRangeValue string, output, errorOutput io.Writer) int {
	return runSummaryWithOptions(commonOptions{logDirectory: logDirectory, timeRange: timeRangeValue}, output, errorOutput)
}

func runSummaryWithOptions(options commonOptions, output, errorOutput io.Writer) int {
	timeRange, err := requestlog.ParseTimeRange(options.timeRange, time.Now())
	if err != nil {
		fmt.Fprintln(errorOutput, "error: invalid --time:", err)
		return 2
	}
	table, err := loadPriceTable(options.priceFile)
	if err != nil {
		fmt.Fprintln(errorOutput, "error:", err)
		return 2
	}
	store, err := requestlog.OpenExisting(options.logDirectory)
	if err != nil {
		fmt.Fprintln(errorOutput, "error:", err)
		return 2
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			fmt.Fprintln(errorOutput, "request log error:", closeErr)
		}
	}()
	var summary requestlog.Summary
	var prices *reportPrices
	if table == nil {
		summary, err = store.Summary(timeRange)
	} else {
		prices = newReportPrices(table)
		summary, err = store.SummaryWithUsage(timeRange, func(usage requestlog.RequestUsage) error {
			prices.addUsage(usage)
			return nil
		})
	}
	if err != nil {
		fmt.Fprintln(errorOutput, "error:", err)
		return 1
	}
	printSummaryWithOptions(output, strings.TrimSpace(options.timeRange), summary, options.humanReadable, prices)
	return 0
}

func printSummary(output io.Writer, timeRangeValue string, summary requestlog.Summary) {
	printSummaryWithOptions(output, timeRangeValue, summary, false, nil)
}

func printSummaryWithOptions(output io.Writer, timeRangeValue string, summary requestlog.Summary, humanReadable bool, prices *reportPrices) {
	if timeRangeValue == "" {
		timeRangeValue = "all"
	}
	fmt.Fprintf(output, "Usage Summary (Range: %s)\n\n", timeRangeValue)
	printPriceContext(output, prices)
	numberFormatter := formatNumber
	if humanReadable {
		numberFormatter = formatHumanNumber
	}
	printRequestCounts(output, summary, numberFormatter, nil)

	rows := make([][]string, 0, len(summary.ByModel)+1)
	rows = append(rows, tokenUsageRowWithFormatter("All models", summary.Total, numberFormatter))
	for _, model := range summary.ByModel {
		rows = append(rows, tokenUsageRowWithFormatter(model.Model, model.Aggregate, numberFormatter))
	}
	fmt.Fprintln(output, "\nToken Usage")
	headers, rows := appendModelPrices([]string{"Model", "Total", "Input", "Cache Write", "Cache Read", "Output", "Reasoning"}, rows, prices)
	printASCIITable(output, headers, rows)
	printPriceNote(output, prices)
}

func printRequestCounts(output io.Writer, summary requestlog.Summary, numberFormatter func(int64) string, prices *reportPrices) {
	rows := make([][]string, 0, len(summary.ByModel)+1)
	rows = append(rows, requestCountRowWithFormatter("All models", summary.Total, numberFormatter))
	for _, model := range summary.ByModel {
		rows = append(rows, requestCountRowWithFormatter(model.Model, model.Aggregate, numberFormatter))
	}
	fmt.Fprintln(output, "Request Counts")
	headers, rows := appendModelPrices([]string{"Model", "Requests", "Success", "Failed", "Canceled", "Unfinished"}, rows, prices)
	printASCIITable(output, headers, rows)
}

func requestCountRow(model string, aggregate requestlog.Aggregate) []string {
	return requestCountRowWithFormatter(model, aggregate, formatNumber)
}

func requestCountRowWithFormatter(model string, aggregate requestlog.Aggregate, numberFormatter func(int64) string) []string {
	return []string{
		model,
		numberFormatter(aggregate.Requests),
		numberFormatter(aggregate.Successes),
		numberFormatter(aggregate.Failures),
		numberFormatter(aggregate.Canceled),
		numberFormatter(aggregate.InProgress),
	}
}

func tokenUsageRow(model string, aggregate requestlog.Aggregate) []string {
	return tokenUsageRowWithFormatter(model, aggregate, formatNumber)
}

func tokenUsageRowWithFormatter(model string, aggregate requestlog.Aggregate, numberFormatter func(int64) string) []string {
	return []string{
		model,
		numberFormatter(aggregate.InputTokens + aggregate.OutputTokens),
		numberFormatter(aggregate.InputTokens),
		numberFormatter(aggregate.CacheWriteTokens),
		numberFormatter(aggregate.CacheReadTokens),
		numberFormatter(aggregate.OutputTokens),
		numberFormatter(aggregate.ReasoningTokens),
	}
}

func printASCIITable(output io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	rightAligned := make([]bool, len(headers))
	for index, header := range headers {
		widths[index] = len(header)
		rightAligned[index] = index > 0 && header != "Price"
	}
	for _, row := range rows {
		for index, cell := range row {
			if index < len(widths) && len(cell) > widths[index] {
				widths[index] = len(cell)
			}
		}
	}

	printASCIITableBorder(output, widths)
	printASCIITableRow(output, headers, widths, nil)
	printASCIITableBorder(output, widths)
	for _, row := range rows {
		printASCIITableRow(output, row, widths, rightAligned)
	}
	printASCIITableBorder(output, widths)
}

func printASCIITableBorder(output io.Writer, widths []int) {
	fmt.Fprint(output, "+")
	for _, width := range widths {
		fmt.Fprint(output, strings.Repeat("-", width+2), "+")
	}
	fmt.Fprintln(output)
}

func printASCIITableRow(output io.Writer, cells []string, widths []int, rightAligned []bool) {
	fmt.Fprint(output, "|")
	for index, width := range widths {
		cell := ""
		if index < len(cells) {
			cell = cells[index]
		}
		padding := strings.Repeat(" ", width-len(cell))
		if index < len(rightAligned) && rightAligned[index] {
			fmt.Fprintf(output, " %s%s |", padding, cell)
			continue
		}
		fmt.Fprintf(output, " %s%s |", cell, padding)
	}
	fmt.Fprintln(output)
}

func formatNumber(value int64) string {
	text := strconv.FormatInt(value, 10)
	signLength := 0
	if strings.HasPrefix(text, "-") {
		signLength = 1
	}
	digitCount := len(text) - signLength
	if digitCount <= 3 {
		return text
	}

	firstGroupLength := digitCount % 3
	if firstGroupLength == 0 {
		firstGroupLength = 3
	}
	var result strings.Builder
	result.Grow(len(text) + (digitCount-1)/3)
	result.WriteString(text[:signLength+firstGroupLength])
	for index := signLength + firstGroupLength; index < len(text); index += 3 {
		result.WriteByte(',')
		result.WriteString(text[index : index+3])
	}
	return result.String()
}

func formatHumanNumber(value int64) string {
	const unitBase = uint64(1000)
	units := [...]string{"", "K", "M", "B", "T", "P", "E"}

	if value > -1000 && value < 1000 {
		return strconv.FormatInt(value, 10)
	}

	negative := value < 0
	magnitude := uint64(value)
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	}

	unitIndex := 0
	divisor := uint64(1)
	for unitIndex < len(units)-1 && magnitude >= divisor*unitBase {
		divisor *= unitBase
		unitIndex++
	}

	for {
		whole := magnitude / divisor
		remainder := magnitude % divisor
		hundredths := (remainder + divisor/200) / (divisor / 100)
		if hundredths == 100 {
			whole++
			hundredths = 0
		}
		if whole == unitBase && unitIndex < len(units)-1 {
			divisor *= unitBase
			unitIndex++
			continue
		}

		result := strconv.FormatUint(whole, 10)
		if hundredths != 0 {
			if hundredths < 10 {
				result += ".0" + strconv.FormatUint(hundredths, 10)
			} else if hundredths%10 == 0 {
				result += "." + strconv.FormatUint(hundredths/10, 10)
			} else {
				result += "." + strconv.FormatUint(hundredths, 10)
			}
		}
		if negative {
			result = "-" + result
		}
		return result + units[unitIndex]
	}
}

func resolveListenAddress(cliAddress, environmentAddress string) string {
	if strings.TrimSpace(cliAddress) != "" {
		return cliAddress
	}
	if strings.TrimSpace(environmentAddress) != "" {
		return environmentAddress
	}
	return "127.0.0.1:8317"
}

func isLoopbackAddress(address string) bool {
	return strings.HasPrefix(address, "127.") || strings.HasPrefix(address, "[::1]") || strings.HasPrefix(address, "localhost:")
}
