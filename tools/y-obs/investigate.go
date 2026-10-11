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
func runInvestigation(ctx context.Context, c *sessionCommand, args []string, tables []string, ns, query, outTable string, stdout, stderr io.Writer, now time.Time) error {
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
	if n := s.planned(); n > c.sf.maxObjects {
		return fail(exitBudget, "the plan reads %d objects, over --max-objects %d (%s; %s): shorten the window, add -n or --node, or raise --max-objects", n, c.sf.maxObjects, s.planSummary(), s.sourcesSummary())
	}
	if c.sf.explain {
		fmt.Fprintln(stdout, s.sourcesSummary())
		return s.printPlan(stdout)
	}
	if err := s.createWindowViews(ctx); err != nil {
		return err
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
	if n == 0 {
		return &exitError{code: exitNoRows}
	}
	return nil
}
