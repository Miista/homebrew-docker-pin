package croncal

import (
	"strings"
	"testing"
	"time"
)

func TestTranslate(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"0 6 * * 1", "Mon *-*-* 06:00:00"},
		{"* * * * *", "*-*-* *:*:00"},
		{"30 4 * * *", "*-*-* 04:30:00"},
		{"0 0 1 * *", "*-*-01 00:00:00"},
		{"15 14 1 * *", "*-*-01 14:15:00"},
		{"0 22 * * 1-5", "Mon,Tue,Wed,Thu,Fri *-*-* 22:00:00"},
		{"0 6 * * MON", "Mon *-*-* 06:00:00"},
		{"0 6 * * mon", "Mon *-*-* 06:00:00"},
		{"0 6 * * SUN", "Sun *-*-* 06:00:00"},
		{"0 6 * * 0", "Sun *-*-* 06:00:00"},
		{"0 6 * * 7", "Sun *-*-* 06:00:00"},
		{"0 6 * * 0,7", "Sun *-*-* 06:00:00"},
		{"0 6 * * 1,3,5", "Mon,Wed,Fri *-*-* 06:00:00"},
		{"0 6 * * MON,WED,FRI", "Mon,Wed,Fri *-*-* 06:00:00"},
		{"0 */6 * * *", "*-*-* 00,06,12,18:00:00"},
		{"*/15 * * * *", "*-*-* *:00,15,30,45:00"},
		{"0 0 * JAN *", "*-01-* 00:00:00"},
		{"0 0 * jan,jul *", "*-01,07-* 00:00:00"},
		{"0 0 * 1-3 *", "*-01,02,03-* 00:00:00"},
		{"0 0 1,15 * *", "*-*-01,15 00:00:00"},
		{"0 0 */10 * *", "*-*-01,11,21,31 00:00:00"},
		{"5 0 * 8 *", "*-08-* 00:05:00"},
		{"0 8-10 * * *", "*-*-* 08,09,10:00:00"},
		{"0 8-18/4 * * *", "*-*-* 08,12,16:00:00"},
		{"0 6 * * MON-FRI", "Mon,Tue,Wed,Thu,Fri *-*-* 06:00:00"},
		{"0 6 * DEC SAT,SUN", "Sun,Sat *-12-* 06:00:00"},
		{"1,1,1 6 * * *", "*-*-* 06:01:00"}, // duplicates collapse
		{"5/20 * * * *", "*-*-* *:05,25,45:00"},
	}
	for _, tt := range tests {
		got, err := Translate(tt.expr)
		if err != nil {
			t.Errorf("Translate(%q): unexpected error: %v", tt.expr, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Translate(%q) = %q, want %q", tt.expr, got, tt.want)
		}
	}
}

func TestNext(t *testing.T) {
	mustParse := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatalf("bad fixture time %q: %v", s, err)
		}
		return tm
	}
	tests := []struct {
		expr string
		from string
		want string
	}{
		{"0 6 * * *", "2026-08-15 05:00", "2026-08-15 06:00"},
		{"0 6 * * *", "2026-08-15 06:00", "2026-08-16 06:00"},
		{"30 4 * * *", "2026-08-15 04:29", "2026-08-15 04:30"},
		{"0 0 1 * *", "2026-08-15 00:00", "2026-09-01 00:00"},
		{"0 22 * * 1-5", "2026-08-15 00:00", "2026-08-17 22:00"}, // Sat -> Mon
		{"*/15 * * * *", "2026-08-15 10:01", "2026-08-15 10:15"},
		// day-of-month OR day-of-week (cron semantics unlike Translate).
		{"0 0 1,15 * MON", "2026-08-15 00:00", "2026-08-17 00:00"}, // 15th already passed; next Monday
	}
	for _, tt := range tests {
		got, err := Next(tt.expr, mustParse(tt.from))
		if err != nil {
			t.Errorf("Next(%q, %q): unexpected error: %v", tt.expr, tt.from, err)
			continue
		}
		if want := mustParse(tt.want); !got.Equal(want) {
			t.Errorf("Next(%q, %q) = %s, want %s", tt.expr, tt.from, got, want)
		}
	}
}

func TestNextRejectsInvalidExpr(t *testing.T) {
	_, err := Next("bad", time.Now())
	if err == nil {
		t.Fatal("Next with invalid expression: expected error, got nil")
	}
}

func TestTranslateRejects(t *testing.T) {
	tests := []struct {
		expr    string
		errPart string
	}{
		{"", "5 fields"},
		{"0 6 * *", "5 fields"},
		{"0 6 * * 1 extra", "5 fields"},
		{"0 6 1 * 1", "both day-of-month and day-of-week"}, // cron OR vs systemd AND
		{"0 6 1-5 * MON", "both day-of-month and day-of-week"},
		{"60 * * * *", "out of range"},
		{"* 24 * * *", "out of range"},
		{"* * 0 * *", "out of range"},
		{"* * 32 * *", "out of range"},
		{"* * * 13 *", "out of range"},
		{"* * * * 8", "out of range"},
		{"x * * * *", "invalid value"},
		{"* * * FOO *", "invalid value"},
		{"* * * * MONDAY", "invalid value"},
		{"5-1 * * * *", "reversed"},
		{"*/0 * * * *", "invalid step"},
		{"*/x * * * *", "invalid step"},
		{"1,,2 * * * *", "empty list item"},
		{"* * * JAN-FOO *", "invalid value"},
	}
	for _, tt := range tests {
		got, err := Translate(tt.expr)
		if err == nil {
			t.Errorf("Translate(%q) = %q, want error containing %q", tt.expr, got, tt.errPart)
			continue
		}
		if !strings.Contains(err.Error(), tt.errPart) {
			t.Errorf("Translate(%q) error = %q, want it to contain %q", tt.expr, err, tt.errPart)
		}
	}
}
