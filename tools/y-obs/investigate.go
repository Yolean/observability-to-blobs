package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"
)

// runInvestigation runs a command's query over the window views of tables:
// plan, budget, --explain, then the rows printed or kept with --out. ns
// prunes the daily tier's namespace partitions.
func runInvestigation(ctx context.Context, c *sessionCommand, args []string, tables []string, ns, query, outTable string, stdout, stderr io.Writer, now time.Time, hooks ...hook) error {
	var h hook
	if len(hooks) > 0 {
		h = hooks[0]
	}
	if c.of.db != "" {
		return fail(exitUsage, "--db is for y-obs sql; to join a saved table with this command's rows, save them with --out and query both with sql")
	}
	w, err := c.sf.window(now)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	s, err := openSession(ctx, &c.sf, w)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.planWindow(ctx, tables, ns, now); err != nil {
		return err
	}
	over := s.cost() > c.sf.maxObjects
	if c.sf.explain {
		// the plan is worth seeing most when it is too big
		fmt.Fprintln(stdout, s.sourcesSummary())
		s.printPlan(stdout)
		if over {
			return s.overBudget(c.sf.maxObjects, "the plan")
		}
		return nil
	}
	if over {
		return s.overBudget(c.sf.maxObjects, "the plan")
	}
	if err := s.createWindowViews(ctx); err != nil {
		return err
	}
	// read the window once: a temp table shadows the view of its name, so
	// the query and the hooks read the scoped rows, not the objects again
	for _, t := range tables {
		if cond, ok := h.scope[t]; ok {
			if err := s.exec(ctx, "CREATE TEMP TABLE w_"+t+" AS SELECT * FROM main.w_"+t+" WHERE "+cond); err != nil {
				return err
			}
		}
	}
	for _, msg := range s.warnings {
		fmt.Fprintln(stderr, "y-obs: "+msg)
	}
	if c.of.attach != "" {
		target := c.of.out
		if err := s.attachLocal(ctx, c.of.attach, target != "" && samePath(c.of.attach, target)); err != nil {
			return err
		}
		s.local = c.of.attach
	}
	if c.of.out != "" {
		t, err := parseOut(c.of.out, outTable)
		if err != nil {
			return err
		}
		q := query
		if c.limitSet() {
			q = limited(query, c.limit)
		}
		n, err := s.writeOut(ctx, t, q, c.of.append, commandLine(c.fs.Name(), args), stdout)
		if err != nil {
			return err
		}
		s.summary(stderr, int(n), false, 0)
		if n == 0 {
			if s.noObjects() {
				return s.explainEmpty(ctx)
			}
			return &exitError{code: exitNoRows}
		}
		return nil
	}
	rows, err := s.conn.QueryContext(ctx, query)
	if err != nil {
		return s.wrap(err)
	}
	defer rows.Close()
	out := bufio.NewWriter(stdout)
	n, more, err := writeRows(out, rows, c.format, c.limit)
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		return s.wrap(err)
	}
	s.summary(stderr, n, more, c.limit)
	if h.cut != nil && n > 0 {
		if note := h.cut(ctx, s, n); note != "" {
			fmt.Fprintln(stderr, "y-obs: "+note)
		}
	}
	if n == 0 {
		if s.noObjects() {
			// nothing in the window at all: say what the bucket has
			return s.explainEmpty(ctx)
		}
		if h.empty != nil {
			if err := h.empty(ctx, s); err != nil {
				return err
			}
		}
		return &exitError{code: exitNoRows}
	}
	return nil
}

// hook lets a command say more than the generic runner: what a cut left
// out, and why a result is empty (README.md, rules 1 and 5).
type hook struct {
	// cut returns a note when the command's own bound (logs --tail) left
	// rows out, given the rows printed
	cut func(ctx context.Context, s *session, printed int) string
	// empty explains an empty result, for example with exit 4 and the
	// names that do exist; nil error keeps exit 1
	empty func(ctx context.Context, s *session) error
	// scope, per table, is a condition that every query of the command
	// (its own and the hooks') keeps to: the window's rows are read once,
	// into a temp table of the rows that meet it
	scope map[string]string
}

// strings runs a query of one text column.
func (s *session) strings(ctx context.Context, q string) ([]string, error) {
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return nil, s.wrap(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v *string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if v != nil {
			out = append(out, *v)
		}
	}
	return out, rows.Err()
}
