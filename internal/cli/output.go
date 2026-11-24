package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// render writes v as JSON with --json, and otherwise as a table: a header
// line and one line per row.
func render(w io.Writer, opts *Options, v any, header []string, rows [][]string) error {
	if opts.JSON {
		enc := json.NewEncoder(w)
		return enc.Encode(v)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func timeOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(time.DateTime)
}
