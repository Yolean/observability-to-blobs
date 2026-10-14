package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/duration"
)

// The API server prints the Table that kubectl get shows, and computes
// its age columns from its own clock: in a state at T they would count
// from now. ageColumns are the columns that count from a time to now:
// the built-in printers' and a CRD's date columns.
var ageColumns = map[string]bool{"Age": true, "Last Seen": true, "First Seen": true, "Last Schedule": true}

// humanDuration matches duration.HumanDuration's output: 45s, 6h24m, 3d4h, 2y10d.
var humanDuration = regexp.MustCompile(`^(?:(\d+)y)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// isTable says whether a response can be a Table: kubectl get asks for
// one (Accept: application/json;as=Table;v=v1;g=meta.k8s.io,...), and the
// API server answers with plain application/json. The body says.
func isTable(r *http.Response) bool {
	return strings.Contains(r.Request.Header.Get("Accept"), "as=Table") &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.Header.Get("Content-Encoding") == ""
}

// ageTablesAt rewrites a Table response's age columns to count to at
// instead of to now: Age from the row's creationTimestamp, the other
// columns shifted back by now minus at.
func ageTablesAt(at time.Time, now func() time.Time) func(*http.Response) error {
	return func(r *http.Response) error {
		if r.StatusCode != http.StatusOK || !isTable(r) || r.Request.URL.Query().Get("watch") == "true" {
			return nil
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return err
		}
		if out, ok := rewriteAges(body, at, now()); ok {
			body = out
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
}

func rewriteAges(body []byte, at, now time.Time) ([]byte, bool) {
	// a generic decode keeps every other field as it came
	var t map[string]any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&t); err != nil || t["kind"] != "Table" {
		return nil, false
	}
	cols, _ := t["columnDefinitions"].([]any)
	rows, _ := t["rows"].([]any)
	shift := now.Sub(at)
	changed := false
	for c, col := range cols {
		def, _ := col.(map[string]any)
		name, _ := def["name"].(string)
		if !ageColumns[name] && def["type"] != "date" {
			continue
		}
		for _, r := range rows {
			row, _ := r.(map[string]any)
			cells, _ := row["cells"].([]any)
			if c >= len(cells) {
				continue
			}
			cell, ok := cells[c].(string)
			if !ok {
				continue
			}
			v := ""
			if name == "Age" {
				v = ageFromObject(row["object"], at)
			}
			if v == "" {
				v = shiftDuration(cell, shift)
			}
			if v != "" && v != cell {
				cells[c] = v
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if err := e.Encode(t); err != nil {
		return nil, false
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}

// ageFromObject is at minus the row object's creationTimestamp, "" when
// the row carries no object.
func ageFromObject(obj any, at time.Time) string {
	o, _ := obj.(map[string]any)
	md, _ := o["metadata"].(map[string]any)
	ts, _ := md["creationTimestamp"].(string)
	created, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return humanSince(created, at)
}

// shiftDuration takes shift off a printed duration, "" when the cell is
// not one (<unknown>, <none>).
func shiftDuration(cell string, shift time.Duration) string {
	m := humanDuration.FindStringSubmatch(cell)
	if m == nil || cell == "" {
		return ""
	}
	var d time.Duration
	for i, unit := range []time.Duration{365 * 24 * time.Hour, 24 * time.Hour, time.Hour, time.Minute, time.Second} {
		n, _ := strconv.Atoi(m[i+1])
		d += time.Duration(n) * unit
	}
	// whole seconds: the printed value was floored from exact times
	return humanSince(time.Time{}, time.Time{}.Add((d - shift).Round(time.Second)))
}

// humanSince prints at minus t as the API server does, <invalid> for a
// time after at.
func humanSince(t, at time.Time) string {
	d := at.Sub(t)
	if d < 0 {
		return "<invalid>"
	}
	return duration.HumanDuration(d)
}
