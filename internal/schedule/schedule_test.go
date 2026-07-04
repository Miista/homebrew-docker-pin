package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindFile(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		want     string
		wantErr  bool
	}{
		{"yaml", []string{"pin.yaml"}, "pin.yaml", false},
		{"yml", []string{"pin.yml"}, "pin.yml", false},
		{"prefers yaml", []string{"pin.yaml", "pin.yml"}, "pin.yaml", false},
		{"missing", nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			compose := writeFile(t, dir, "docker-compose.yml", "services: {}\n")
			for _, f := range tt.files {
				writeFile(t, dir, f, "schedule: \"0 6 * * 1\"\n")
			}
			got, err := FindFile(compose)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != filepath.Join(dir, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Config
		errPart string
	}{
		{
			name:    "full",
			content: "schedule: \"0 6 * * 1\"\nservices:\n  - caddy\n  - cloudflared\non_change: ./pin-upgraded.sh\n",
			want:    Config{Schedule: "0 6 * * 1", Services: []string{"caddy", "cloudflared"}, OnChange: "./pin-upgraded.sh"},
		},
		{
			name:    "minimal",
			content: "schedule: \"30 4 * * *\"\n",
			want:    Config{Schedule: "30 4 * * *"},
		},
		{
			name:    "missing schedule",
			content: "services:\n  - caddy\n",
			errPart: "'schedule' is required",
		},
		{
			name:    "bad yaml",
			content: "schedule: [\n",
			errPart: "parsing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "pin.yaml", tt.content)
			cfg, err := Load(p)
			if tt.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errPart) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.errPart)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Schedule != tt.want.Schedule || cfg.OnChange != tt.want.OnChange ||
				strings.Join(cfg.Services, ",") != strings.Join(tt.want.Services, ",") {
				t.Errorf("got %+v, want %+v", *cfg, tt.want)
			}
		})
	}
}

func TestSlug(t *testing.T) {
	a := Slug("/srv/compose/caddy")
	b := Slug("/home/user/caddy")
	if a == b {
		t.Errorf("different paths with same basename must not collide: %q", a)
	}
	if !strings.HasPrefix(a, "caddy-") {
		t.Errorf("slug should start with basename: %q", a)
	}
	if a != Slug("/srv/compose/caddy") {
		t.Error("slug must be deterministic")
	}
	if got := Slug("/srv/My App!"); !strings.HasPrefix(got, "my-app-") {
		t.Errorf("slug should sanitize basename, got %q", got)
	}
}

func TestUnitNames(t *testing.T) {
	svc, tmr := UnitNames("/srv/caddy")
	if !strings.HasPrefix(svc, "docker-pin-caddy-") || !strings.HasSuffix(svc, ".service") {
		t.Errorf("bad service unit name %q", svc)
	}
	if !strings.HasPrefix(tmr, "docker-pin-caddy-") || !strings.HasSuffix(tmr, ".timer") {
		t.Errorf("bad timer unit name %q", tmr)
	}
}

func TestUnits(t *testing.T) {
	cfg := &Config{Schedule: "0 6 * * 1"}
	svc, tmr, err := Units(cfg, "/srv/caddy", "/usr/libexec/docker/cli-plugins/docker-pin", "soren")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Type=oneshot",
		"User=soren",
		"WorkingDirectory=/srv/caddy",
		"ExecStart=/usr/libexec/docker/cli-plugins/docker-pin schedule run",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service unit missing %q:\n%s", want, svc)
		}
	}
	for _, want := range []string{
		"OnCalendar=Mon *-*-* 06:00:00",
		"Persistent=true",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(tmr, want) {
			t.Errorf("timer unit missing %q:\n%s", want, tmr)
		}
	}
}

func TestUnitsBadCron(t *testing.T) {
	_, _, err := Units(&Config{Schedule: "0 6 1 * 1"}, "/srv/caddy", "/bin/docker-pin", "root")
	if err == nil {
		t.Fatal("want error for cron with both day-of-month and day-of-week restricted")
	}
}
