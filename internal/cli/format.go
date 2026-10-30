package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(w io.Writer, header string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// state is a run's, job's or step's status in a word: its conclusion once
// it's completed.
func state(status, conclusion string) string {
	if status == "completed" && conclusion != "" {
		return conclusion
	}
	return status
}

var stateIcons = map[string]string{
	"success": "✓", "failure": "✗", "cancelled": "⊘", "skipped": "-",
	"in_progress": "●", "queued": "○", "pending": "○", "waiting": "○",
}

// stateLabel is state with an icon.
func stateLabel(status, conclusion string) string {
	s := state(status, conclusion)
	if icon, ok := stateIcons[s]; ok {
		return icon + " " + s
	}
	return s
}

func ago(ms int64) string {
	if ms == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func timestamp(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

// elapsed is how long something took, or has taken so far.
func elapsed(start, end int64) string {
	if start == 0 {
		return "-"
	}
	if end == 0 {
		end = time.Now().UnixMilli()
	}
	return (time.Duration(end-start) * time.Millisecond).Round(time.Second).String()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func shortRef(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if r, ok := strings.CutPrefix(ref, p); ok {
			return r
		}
	}
	return ref
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// stripANSI removes terminal colours and the like from log text.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRE.ReplaceAllString(s, "")
}
