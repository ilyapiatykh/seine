package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		"empty default": {"", slog.LevelInfo, false},
		"info":          {"info", slog.LevelInfo, false},
		"debug":         {"DEBUG", slog.LevelDebug, false},
		"warn":          {"warn", slog.LevelWarn, false},
		"warning":       {"warning", slog.LevelWarn, false},
		"error":         {"error", slog.LevelError, false},
		"unknown":       {"verbose", 0, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseLevel(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	cases := map[string]struct {
		in      string
		want    Format
		wantErr bool
	}{
		"empty default": {"", FormatText, false},
		"text":          {"text", FormatText, false},
		"json":          {"JSON", FormatJSON, false},
		"unknown":       {"yaml", "", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseFormat(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetupRespectsFormatAndLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(Options{
		Level:  slog.LevelWarn,
		Format: FormatJSON,
		Output: &buf,
	})
	logger.Info("filtered out")
	logger.Warn("kept", slog.String("k", "v"))
	out := buf.String()
	if strings.Contains(out, "filtered out") {
		t.Fatalf("info-level message leaked at warn level: %s", out)
	}
	if !strings.Contains(out, `"msg":"kept"`) || !strings.Contains(out, `"k":"v"`) {
		t.Fatalf("expected JSON record with kept message and k=v, got: %s", out)
	}
}
