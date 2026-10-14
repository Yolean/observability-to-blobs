package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// timeLayout is every timestamp y-obs prints: UTC, nine fractional digits,
// as the CRI writes them and the logs API returns them.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

var formats = []string{"text", "tsv", "csv", "json", "jsonl"}

// plain turns an engine value into one JSON can carry: times as strings
// in timeLayout, maps with their keys as strings, UUIDs and blobs as text.
func plain(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(timeLayout)
	case []byte:
		return hex.EncodeToString(x)
	case duckdb.UUID:
		return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
	case *duckdb.UUID:
		if x == nil {
			return nil
		}
		return plain(*x)
	case duckdb.OrderedMap:
		return plain(&x)
	case *duckdb.OrderedMap:
		if x == nil {
			return nil
		}
		m := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			m[fmt.Sprint(plain(k))] = plain(val)
		}
		return m
	case duckdb.Map:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(plain(k))] = plain(val)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = plain(val)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, val := range x {
			l[i] = plain(val)
		}
		return l
	case duckdb.Interval:
		return fmt.Sprintf("%d months %d days %d us", x.Months, x.Days, x.Micros)
	case duckdb.Decimal:
		return x.String()
	case *big.Int:
		return x.String()
	case duckdb.Union:
		return plain(x.Value)
	}
	return v
}

// text renders a value in one TSV or CSV cell: scalars as themselves,
// composites as JSON, NULL as the empty string.
func text(v any) string {
	p := plain(v)
	switch x := p.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	if f, ok := p.(float64); ok {
		// plain decimals where they stay readable: 77094912, not 7.7094912e+07
		if a := math.Abs(f); a == 0 || a >= 1e-6 && a < 1e15 {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	return fmt.Sprint(p)
}

// tsvEscape keeps one row per line: backslash, tab, newline and carriage
// return become \\, \t, \n and \r, as in PostgreSQL's text format.
var tsvEscape = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// writeRows prints at most limit rows (0: every row) in a format and
// returns how many it printed and whether more were left.
func writeRows(w io.Writer, rows *sql.Rows, format string, limit int) (n int, more bool, err error) {
	cols, err := rows.Columns()
	if err != nil {
		return 0, false, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var cw *csv.Writer
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	switch format {
	case "tsv":
		fmt.Fprintln(w, strings.Join(cols, "\t"))
	case "text":
		// no header: one line per row, the columns joined by spaces
	case "csv":
		cw = csv.NewWriter(w)
		if err := cw.Write(cols); err != nil {
			return 0, false, err
		}
	case "json":
		fmt.Fprint(w, "[")
	}
	for rows.Next() {
		if limit > 0 && n == limit {
			more = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, false, err
		}
		switch format {
		case "tsv":
			cells := make([]string, len(vals))
			for i, v := range vals {
				cells[i] = tsvEscape.Replace(text(v))
			}
			fmt.Fprintln(w, strings.Join(cells, "\t"))
		case "text":
			cells := make([]string, 0, len(vals))
			for _, v := range vals {
				if v != nil {
					cells = append(cells, text(v))
				}
			}
			fmt.Fprintln(w, strings.Join(cells, " "))
		case "csv":
			cells := make([]string, len(vals))
			for i, v := range vals {
				cells[i] = text(v)
			}
			if err := cw.Write(cells); err != nil {
				return n, false, err
			}
		case "json", "jsonl":
			if format == "json" && n > 0 {
				fmt.Fprint(w, ",")
			}
			obj := orderedRow{cols: cols, vals: make([]any, len(vals))}
			for i, v := range vals {
				obj.vals[i] = plain(v)
			}
			if err := enc.Encode(obj); err != nil {
				return n, false, err
			}
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, more, err
	}
	switch format {
	case "csv":
		cw.Flush()
		return n, more, cw.Error()
	case "json":
		fmt.Fprintln(w, "]")
	}
	return n, more, nil
}

// orderedRow encodes as a JSON object with the columns in query order.
type orderedRow struct {
	cols []string
	vals []any
}

func (r orderedRow) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString("{")
	for i, c := range r.cols {
		if i > 0 {
			b.WriteString(",")
		}
		k, _ := json.Marshal(c)
		v, err := json.Marshal(r.vals[i])
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteString(":")
		b.Write(v)
	}
	b.WriteString("}")
	return []byte(b.String()), nil
}

func validFormat(f string) bool {
	i := sort.SearchStrings(sortedFormats, f)
	return i < len(sortedFormats) && sortedFormats[i] == f
}

var sortedFormats = func() []string {
	s := append([]string(nil), formats...)
	sort.Strings(s)
	return s
}()
