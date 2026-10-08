// rules-manifest extracts, from Prometheus rule files, the exact set of
// series selectors that rule evaluation needs: metric names plus label
// matchers. The output is the consumption contract for blob tailers:
// they (blobs-to-prometheus SELECTORS_FILE) fetch only matching series,
// and the manifest diff in review makes a rule's data dependencies
// visible. Parsing uses Prometheus's own PromQL parser, so
// whatever an expr means to Prometheus it means here.
//
// Accepted inputs (files or directories, recursively for .yml/.yaml, a
// directory's kustomization.yaml left out),
// every document of a multi-document file read:
//   - PrometheusRule objects (spec.groups)
//   - plain {groups: [...]} rule files
//   - bare group lists (as in a directory of custom rule files)
//
// Any error is fatal and nothing is printed: a YAML error, a document of
// none of these shapes, an expr PromQL rejects, or no selectors at all.
// A partial manifest would make the tailer silently drop the series the
// skipped rules need, and an empty one makes it push nothing.
//
// Usage: rules-manifest <path>... > manifest.json
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"
)

type Manifest struct {
	Selectors []Selector `json:"selectors"`
}

// Selector is one vector selector: a metric name (empty when the name
// itself is matched via a __name__ matcher or absent entirely) and its
// label matchers. Matcher types are PromQL's: = != =~ !~
type Selector struct {
	Name     string    `json:"name"`
	Matchers []Matcher `json:"matchers,omitempty"`
}

type Matcher struct {
	Label string `json:"label"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type rule struct {
	Alert  string `yaml:"alert"`
	Record string `yaml:"record"`
	Expr   string `yaml:"expr"`
}

type group struct {
	Name  string `yaml:"name"`
	Rules []rule `yaml:"rules"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rules-manifest <rule file or dir>...")
		os.Exit(2)
	}
	var files []string
	for _, arg := range os.Args[1:] {
		found, err := collect(arg)
		if err != nil {
			fatal(err)
		}
		files = append(files, found...)
	}
	if len(files) == 0 {
		fatal(fmt.Errorf("no .yml or .yaml files in %s", strings.Join(os.Args[1:], " ")))
	}
	m, errs := FromFiles(files)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "rules-manifest:", e)
		}
		fmt.Fprintf(os.Stderr, "rules-manifest: %d error(s), no manifest written\n", len(errs))
		os.Exit(1)
	}
	if len(m.Selectors) == 0 {
		fatal(fmt.Errorf("no selectors in %d file(s); a tailer given this manifest would push nothing", len(files)))
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(out))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "rules-manifest:", err)
	os.Exit(1)
}

// kustomizeFile are the names kustomize reads as a kustomization. A rules
// directory deployed with kustomize has one beside the rule files, and it is
// never a rule document, so a directory walk leaves it out; named as an
// argument it is read, and refused, like any other file.
var kustomizeFile = map[string]bool{"kustomization.yaml": true, "kustomization.yml": true, "Kustomization": true}

// collect lists path, or the .yml and .yaml files under it but kustomizeFile.
func collect(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{path}, nil
	}
	var out []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")) && !kustomizeFile[d.Name()] {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// FromFiles parses every file and returns the deduplicated, sorted union
// of selectors, an empty list (never null) when there are none. Unreadable
// files, YAML errors and unparseable exprs are collected as errors naming
// the file, never silently dropped: a dropped rule would otherwise mean
// silently missing data.
func FromFiles(files []string) (Manifest, []error) {
	var errs []error
	seen := map[string]Selector{}
	pql := parser.NewParser(parser.Options{})
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		groups, err := parseGroups(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		for _, g := range groups {
			for _, r := range g.Rules {
				if r.Expr == "" {
					continue
				}
				expr, err := pql.ParseExpr(r.Expr)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %s: %w", f, ruleName(r), err))
					continue
				}
				for _, sel := range parser.ExtractSelectors(expr) {
					s := fromMatchers(sel)
					seen[key(s)] = s
				}
			}
		}
	}
	m := Manifest{Selectors: []Selector{}}
	for _, s := range seen {
		m.Selectors = append(m.Selectors, s)
	}
	sort.Slice(m.Selectors, func(i, j int) bool { return key(m.Selectors[i]) < key(m.Selectors[j]) })
	return m, errs
}

func ruleName(r rule) string {
	if r.Alert != "" {
		return "alert " + r.Alert
	}
	return "record " + r.Record
}

// parseGroups reads every document in data, each one of the three shapes
// named in the package comment. Empty documents (a bare "---", a file of
// comments) are skipped; any other document must be a rule document.
func parseGroups(data []byte) ([]group, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []group
	for n := 1; ; n++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}
		groups, err := docGroups(&doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}
		out = append(out, groups...)
	}
}

func docGroups(doc *yaml.Node) ([]group, error) {
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	switch {
	case root.Kind == yaml.ScalarNode && root.Tag == "!!null":
		return nil, nil
	case root.Kind == yaml.SequenceNode:
		var bare []group
		if err := root.Decode(&bare); err != nil {
			return nil, err
		}
		return bare, nil
	case root.Kind == yaml.MappingNode:
		var d struct {
			Spec *struct {
				Groups *[]group `yaml:"groups"`
			} `yaml:"spec"`
			Groups *[]group `yaml:"groups"`
		}
		if err := root.Decode(&d); err != nil {
			return nil, err
		}
		if d.Spec != nil && d.Spec.Groups != nil {
			return *d.Spec.Groups, nil
		}
		if d.Groups != nil {
			return *d.Groups, nil
		}
	}
	return nil, fmt.Errorf("line %d: not a rule document (want spec.groups, groups, or a list of groups)", root.Line)
}

func fromMatchers(ms []*labels.Matcher) Selector {
	s := Selector{}
	for _, m := range ms {
		if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
			s.Name = m.Value
			continue
		}
		s.Matchers = append(s.Matchers, Matcher{Label: m.Name, Type: m.Type.String(), Value: m.Value})
	}
	sort.Slice(s.Matchers, func(i, j int) bool {
		return s.Matchers[i].Label+s.Matchers[i].Type+s.Matchers[i].Value <
			s.Matchers[j].Label+s.Matchers[j].Type+s.Matchers[j].Value
	})
	return s
}

func key(s Selector) string {
	b, _ := json.Marshal(s)
	return string(b)
}
