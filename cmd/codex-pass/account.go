package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"codex-pass/internal/auth"
	"codex-pass/internal/codexapi"
)

type accountFile struct {
	name   string
	client *codexapi.AccountClient
}

func runAccountCommand(ctx context.Context, options commandLine, httpClient *http.Client, input io.Reader, output, errorOutput io.Writer) int {
	credentials, err := auth.LoadAll(options.authFiles)
	if err != nil {
		fmt.Fprintln(errorOutput, "error:", err)
		return 2
	}
	fileNames := make(map[string]int, len(credentials))
	for _, credential := range credentials {
		fileNames[filepath.Base(credential.Path())]++
	}
	accounts := make([]accountFile, 0, len(credentials))
	for _, credential := range credentials {
		path := credential.Path()
		name := filepath.Base(path)
		if fileNames[name] > 1 {
			name = path
		}
		accounts = append(accounts, accountFile{name: name, client: codexapi.NewAccountClient(credential, httpClient)})
	}
	if options.command == "reset" {
		if err := runReset(ctx, accounts, options.account.resetNumber, input, output, errorOutput); err != nil {
			fmt.Fprintln(errorOutput, "error:", err)
			return 1
		}
		return 0
	}

	exitCode := 0
	for index, account := range accounts {
		if index > 0 {
			fmt.Fprintln(output)
		}
		if len(accounts) > 1 {
			fmt.Fprintf(output, "AUTH_FILE %s\n", account.name)
		}
		var err error
		switch options.command {
		case "quota":
			err = printWeeklyQuota(ctx, account.client, output)
		case "models":
			err = printAccountModels(ctx, account, options.account.clientVersion, output, errorOutput)
		}
		if err != nil {
			fmt.Fprintf(errorOutput, "error: %s: %v\n", account.name, err)
			exitCode = 1
		}
	}
	return exitCode
}

func printWeeklyQuota(ctx context.Context, client *codexapi.AccountClient, output io.Writer) error {
	usage, err := client.Usage(ctx)
	if err != nil {
		return err
	}
	rows := appendWeeklyQuota(nil, "Codex", usage.RateLimit)
	rows = appendWeeklyQuota(rows, "Code review", usage.CodeReviewLimit)
	for _, limit := range usage.AdditionalLimits {
		name := limit.Name
		if name == "" {
			name = limit.MeteredFeature
		}
		rows = appendWeeklyQuota(rows, name, limit.RateLimit)
	}
	if usage.Plan != "" {
		fmt.Fprintf(output, "Weekly quota (%s)\n", usage.Plan)
	} else {
		fmt.Fprintln(output, "Weekly quota")
	}
	if len(rows) == 0 {
		fmt.Fprintln(output, "The account returned no weekly quota windows.")
		return nil
	}
	printASCIITable(output, []string{"Limit", "Used", "Remaining", "Resets at"}, rows)
	return nil
}

func appendWeeklyQuota(rows [][]string, name string, limit codexapi.RateLimit) [][]string {
	const secondsPerWeek = 7 * 24 * 60 * 60
	for _, window := range []*codexapi.RateLimitWindow{limit.Primary, limit.Secondary} {
		if window == nil || window.Seconds != secondsPerWeek {
			continue
		}
		used, remaining := "-", "-"
		if window.UsedPercent != nil {
			used = formatPercent(*window.UsedPercent)
			remaining = formatPercent(max(0, 100-*window.UsedPercent))
		}
		resets := "-"
		if window.ResetAt > 0 {
			resets = formatAccountTime(time.Unix(window.ResetAt, 0))
		}
		rows = append(rows, []string{name, used, remaining, resets})
	}
	return rows
}

func formatPercent(value float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".") + "%"
}

func formatAccountTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05 -07:00")
}

type accountReset struct {
	account accountFile
	credit  codexapi.ResetCredit
}

func runReset(ctx context.Context, accounts []accountFile, number int, input io.Reader, output, errorOutput io.Writer) error {
	var total int
	var available []accountReset
	var incomplete []string
	for _, account := range accounts {
		count, credits, err := account.client.ResetCredits(ctx)
		if err != nil {
			// A partial list can give the same number to a different credit.
			return fmt.Errorf("read resets for %s: %w", account.name, err)
		}
		total += count
		if count == 0 {
			continue
		}
		credits = slices.DeleteFunc(credits, func(credit codexapi.ResetCredit) bool {
			return credit.Status != "available"
		})
		if len(credits) != count {
			incomplete = append(incomplete, account.name)
		}
		// Keep each file's numbers stable even if its service response is reordered.
		slices.SortFunc(credits, func(a, b codexapi.ResetCredit) int {
			if order := a.GrantedAt.Compare(b.GrantedAt); order != 0 {
				return order
			}
			return strings.Compare(a.ID, b.ID)
		})
		for _, credit := range credits {
			available = append(available, accountReset{account: account, credit: credit})
		}
	}
	if number == 0 {
		printResetCredits(output, total, available, len(incomplete) == 0)
		if len(incomplete) > 0 {
			fmt.Fprintf(output, "Reset numbers require complete credit details for %s.\n", strings.Join(incomplete, ", "))
		}
		return nil
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("reset credit details are incomplete for %s", strings.Join(incomplete, ", "))
	}
	if number < 1 || number > len(available) {
		return fmt.Errorf("reset %d is not in the current list of available credits", number)
	}
	selected := available[number-1]
	credit := selected.credit
	if credit.SupportedByPlan != nil && !*credit.SupportedByPlan {
		return fmt.Errorf("reset %d is unavailable on this plan", number)
	}
	title := credit.Title
	if title == "" {
		title = credit.Type
	}
	if _, err := fmt.Fprintf(output, "Reset %d (%s)\nAUTH_FILE %s\nGranted at %s\nExpires at %s\nRedeem this reset? [y/N] ",
		number, title, selected.account.name, formatAccountTime(credit.GrantedAt), formatAccountTime(credit.ExpiresAt)); err != nil {
		return fmt.Errorf("display reset confirmation: %w", err)
	}
	answer, err := bufio.NewReader(input).ReadSlice('\n')
	fmt.Fprintln(output)
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(answer)), "y") {
		fmt.Fprintln(output, "Reset canceled.")
		return nil
	}
	result, err := selected.account.client.ConsumeResetCredit(ctx, credit.ID)
	if err != nil {
		return fmt.Errorf("could not verify the redemption result. Check the current reset list before trying again: %w", err)
	}
	switch result.Code {
	case "reset":
		fmt.Fprintf(output, "Usage limit reset %d redeemed (%d windows reset).\n", number, result.WindowsReset)
	case "already_redeemed":
		fmt.Fprintf(output, "Usage limit reset %d was already redeemed.\n", number)
	case "nothing_to_reset":
		return fmt.Errorf("no eligible usage window to reset")
	case "no_credit":
		return fmt.Errorf("the selected reset credit is no longer available")
	default:
		return fmt.Errorf("unknown redemption result. Check the current reset list before trying again")
	}
	if err := printWeeklyQuota(ctx, selected.account.client, output); err != nil {
		fmt.Fprintf(errorOutput, "warning: could not read updated quota for %s: %v\n", selected.account.name, err)
	}
	return nil
}

func printResetCredits(output io.Writer, count int, credits []accountReset, numbered bool) {
	fmt.Fprintf(output, "Usage limit resets: %d available\n", count)
	if len(credits) > 0 {
		rows := make([][]string, 0, len(credits))
		for index, entry := range credits {
			number := "-"
			if numbered {
				number = strconv.Itoa(index + 1)
			}
			expiration := formatAccountTime(entry.credit.ExpiresAt)
			if entry.credit.SupportedByPlan != nil && !*entry.credit.SupportedByPlan {
				expiration += " (unavailable on this plan)"
			}
			rows = append(rows, []string{number, entry.account.name, expiration})
		}
		printASCIITable(output, []string{"#", "AUTH_FILE", "Expires at"}, rows)
	}
}

func printAccountModels(ctx context.Context, account accountFile, version string, output, errorOutput io.Writer) error {
	models, err := account.client.CodexModels(ctx, version)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Codex models (client version %s)\n", version)
	rows := make([][]string, 0, len(models))
	for _, model := range models {
		rows = append(rows, []string{model.Slug, model.DisplayName, model.Visibility})
	}
	printASCIITable(output, []string{"Model", "Name", "Visibility"}, rows)

	chatModels, err := account.client.ChatGPTModels(ctx)
	if err != nil {
		fmt.Fprintf(errorOutput, "warning: could not read ChatGPT model catalog for %s: %v\n", account.name, err)
	} else {
		fmt.Fprintln(output, "\nChatGPT models (ChatGPT catalog)")
		rows := make([][]string, 0, len(chatModels))
		for _, model := range chatModels {
			imageTool := "-"
			if model.EnabledTools != nil {
				imageTool = "no"
				if slices.Contains(model.EnabledTools, "image_gen_tool_enabled") {
					imageTool = "yes"
				}
			}
			rows = append(rows, []string{model.Slug, model.Title, imageTool})
		}
		printASCIITable(output, []string{"Model", "Name", "Image tool"}, rows)
	}

	fmt.Fprintln(output, "\nCodex image tool")
	fmt.Fprintln(output, "  gpt-image-2 (documented default, account access unverified)")
	return nil
}
