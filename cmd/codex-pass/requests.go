package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"codex-pass/internal/requestlog"
)

func runRequestLogs(options commonOptions, request requestOptions, output, errorOutput io.Writer) int {
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
	logs, err := store.Logs(timeRange, request.ascending, request.num)
	if err != nil {
		fmt.Fprintln(errorOutput, "error:", err)
		return 1
	}
	printRequestLogs(output, options, request, logs, calculatePrices(table, logs.Entries))
	return 0
}

func printRequestLogs(output io.Writer, options commonOptions, request requestOptions, logs requestlog.RequestLogs, prices *reportPrices) {
	timeRangeValue := strings.TrimSpace(options.timeRange)
	if timeRangeValue == "" {
		timeRangeValue = "all"
	}
	fmt.Fprintf(output, "Request History (Range: %s", timeRangeValue)
	if request.num > 0 {
		fmt.Fprintf(output, ", Limit: %d", request.num)
	}
	fmt.Fprint(output, ")\n\n")
	printPriceContext(output, prices)
	numberFormatter := formatNumber
	if options.humanReadable {
		numberFormatter = formatHumanNumber
	}
	printRequestCounts(output, logs.Summary, numberFormatter, prices)

	headers := []string{"Request time", "Time taken", "Model", "Input", "Output", "Cache hit"}
	if prices != nil {
		headers = append(headers, "Price")
	}
	rows := make([][]string, 0, len(logs.Entries))
	for _, entry := range logs.Entries {
		row := []string{
			entry.StartedAt.In(time.Local).Format("2006-01-02 15:04:05"),
			formatRequestDuration(entry.StartedAt, entry.FinishedAt),
			entry.Model,
			formatRequestTokens(entry.InputTokens, entry.CacheReadTokens, numberFormatter),
			formatRequestTokens(entry.OutputTokens, entry.ReasoningTokens, numberFormatter),
			formatCacheHit(entry.InputTokens, entry.CacheReadTokens),
		}
		if prices != nil {
			row = append(row, prices.table.Format(prices.byRequest[entry.RequestID]))
		}
		rows = append(rows, row)
	}
	if prices != nil {
		rows = append(rows, priceSumRow(len(headers), prices))
	}
	fmt.Fprintln(output, "\nRequest Logs")
	printASCIITable(output, headers, rows)
	printPriceNote(output, prices)
}

func formatRequestTokens(total, detail *int64, numberFormatter func(int64) string) string {
	format := func(value *int64) string {
		if value == nil {
			return "-"
		}
		return numberFormatter(*value)
	}
	return format(total) + " ( " + format(detail) + " )"
}

func formatCacheHit(input, cached *int64) string {
	if input == nil || cached == nil || *input <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", float64(*cached)/float64(*input)*100)
}

func formatRequestDuration(start time.Time, end *time.Time) string {
	if end == nil || end.Before(start) {
		return "-"
	}
	duration := end.Sub(start)
	value, unit := duration.Seconds(), "second"
	if duration >= time.Minute {
		value, unit = duration.Minutes(), "minute"
	}
	text := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
	if text != "1" {
		unit += "s"
	}
	return text + " " + unit
}
