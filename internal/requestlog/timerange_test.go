package requestlog

import (
	"testing"
	"time"
)

func TestParseTimeRange(t *testing.T) {
	originalLocal := time.Local
	location := time.FixedZone("test", 8*60*60)
	time.Local = location
	t.Cleanup(func() { time.Local = originalLocal })
	now := time.Date(2026, time.October, 22, 12, 30, 45, 123456789, location)

	tests := []struct {
		input     string
		wantStart time.Time
		wantEnd   time.Time
		all       bool
	}{
		{
			input:     "",
			all:       true,
			wantStart: time.Time{},
			wantEnd:   time.Time{},
		},
		{
			input:     "2026-06-09,2026-10-10",
			wantStart: time.Date(2026, time.June, 9, 0, 0, 0, 0, location),
			wantEnd:   time.Date(2026, time.October, 11, 0, 0, 0, 0, location),
		},
		{
			input:     "2026-06-09-10-00,2026-10-10-08-45",
			wantStart: time.Date(2026, time.June, 9, 10, 0, 0, 0, location),
			wantEnd:   time.Date(2026, time.October, 10, 8, 46, 0, 0, location),
		},
		{
			input:     "2d",
			wantStart: now.Add(-48 * time.Hour),
			wantEnd:   now.Add(time.Millisecond),
		},
		{
			input:     "1w",
			wantStart: now.Add(-7 * 24 * time.Hour),
			wantEnd:   now.Add(time.Millisecond),
		},
		{
			input:     "3m",
			wantStart: now.AddDate(0, -3, 0),
			wantEnd:   now.Add(time.Millisecond),
		},
		{
			input:     "1y",
			wantStart: now.AddDate(-1, 0, 0),
			wantEnd:   now.Add(time.Millisecond),
		},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseTimeRange(test.input, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.All != test.all || !got.Start.Equal(test.wantStart) || !got.End.Equal(test.wantEnd) {
				t.Fatalf("ParseTimeRange(%q) = %#v, want start=%s end=%s all=%t", test.input, got, test.wantStart, test.wantEnd, test.all)
			}
		})
	}
}

func TestParseTimeRangeRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{
		"0d",
		"2h",
		"2026-10-10",
		"2026-10-10,2026-06-09",
		"2026-13-01,2026-10-10",
		"2026-10-10-10-00,not-a-time",
		"106752d",
		"15251w",
		"100001m",
		"100001y",
	} {
		if _, err := ParseTimeRange(input, time.Now()); err == nil {
			t.Errorf("ParseTimeRange(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseTimeRangeClampsCalendarBoundaries(t *testing.T) {
	location := time.FixedZone("test", 8*60*60)
	tests := []struct {
		name      string
		input     string
		now       time.Time
		wantStart time.Time
	}{
		{
			name:      "month end",
			input:     "1m",
			now:       time.Date(2026, time.March, 31, 10, 30, 0, 0, location),
			wantStart: time.Date(2026, time.February, 28, 10, 30, 0, 0, location),
		},
		{
			name:      "leap day year",
			input:     "1y",
			now:       time.Date(2024, time.February, 29, 10, 30, 0, 0, location),
			wantStart: time.Date(2023, time.February, 28, 10, 30, 0, 0, location),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseTimeRange(test.input, test.now)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Start.Equal(test.wantStart) {
				t.Fatalf("start = %s, want %s", got.Start, test.wantStart)
			}
		})
	}
}
