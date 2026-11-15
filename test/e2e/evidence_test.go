//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The evidence a run collects: every check with what it saw, and every scale
// action the downscaler logged. At the end it is written out as a report, so a
// passing run shows what was verified for each operator rather than staying
// silent.
var (
	evidenceMu  sync.Mutex
	checkProofs []checkProof
	scaleProofs []scaleProof
)

// checkProof is one check: which subtest ran it, what it expected, what it saw.
type checkProof struct {
	test  string
	check string
	seen  string
	took  time.Duration
	ok    bool
}

// scaleProof is one scale action the downscaler reported in its log.
type scaleProof struct {
	at        time.Time
	action    string
	kind      string
	name      string
	attribute string
	from, to  string
	scope     string
}

// recordCheck adds a check result to the evidence.
func recordCheck(t *testing.T, name, seen string, took time.Duration, ok bool) {
	t.Helper()

	evidenceMu.Lock()
	defer evidenceMu.Unlock()

	checkProofs = append(checkProofs, checkProof{
		test:  strings.TrimPrefix(t.Name(), "TestDownscaler/"),
		check: name,
		seen:  seen,
		took:  took,
		ok:    ok,
	})
}

// verify records a one-off check and fails t when it does not hold.
func verify(t *testing.T, name string, ok bool, seen string) {
	t.Helper()
	recordCheck(t, name, seen, 0, ok)

	if !ok {
		t.Fatalf("%s: not true (saw: %s)", name, seen)
	}

	t.Logf("ok:   %s (saw %s)", name, seen)
}

// scaleMessage matches the downscaler's log line for a scale action, e.g.
// "successfully scaled down workload" or "successfully suspended workload".
var scaleMessage = regexp.MustCompile(`^successfully (.+) workload$`)

// recordScaleLine adds the scale action in one JSON log line, if it is one.
func recordScaleLine(line string, at time.Time) {
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return
	}

	match := scaleMessage.FindStringSubmatch(fmt.Sprint(entry["msg"]))
	if match == nil {
		return
	}

	field := func(key string) string {
		if value, ok := entry[key]; ok && value != nil {
			return fmt.Sprint(value)
		}

		return ""
	}

	evidenceMu.Lock()
	defer evidenceMu.Unlock()

	scaleProofs = append(scaleProofs, scaleProof{
		at:        at,
		action:    match[1],
		kind:      field("kind"),
		name:      field("workload"),
		attribute: field("changedAttribute"),
		from:      field("from"),
		to:        field("to"),
		scope:     strings.TrimPrefix(field("decisionScope"), "Scope"),
	})
}

// operators maps what the tests touch to the operator it belongs to, matched
// in order against a check name or "Kind/name".
var operators = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"RabbitMQ", regexp.MustCompile(`(?i)rabbit`)},
	{"Elasticsearch (ECK)", regexp.MustCompile(`(?i)elastic|search`)},
	{"Redis", regexp.MustCompile(`(?i)redis`)},
	{"PostgreSQL (CloudNativePG)", regexp.MustCompile(`(?i)cnpg|\bCluster[ /]pg\b`)},
	{"MongoDB Community", regexp.MustCompile(`(?i)mongo`)},
	{"Deployments", regexp.MustCompile(`(?i)\bweb\b|web-excluded`)},
}

// operatorOf names the operator s is about, or "" for checks about the whole run.
func operatorOf(s string) string {
	for _, op := range operators {
		if op.pattern.MatchString(s) {
			return op.name
		}
	}

	return ""
}

// reportPath is e2e-report.md next to the log file.
func reportPath() string {
	return filepath.Join(filepath.Dir(logFilePath()), "e2e-report.md")
}

// writeEvidenceReport prints the evidence as Markdown and writes it to
// reportPath. passed is the overall result of the test.
func writeEvidenceReport(t *testing.T, passed bool) {
	t.Helper()

	evidenceMu.Lock()
	checks := append([]checkProof(nil), checkProofs...)
	scales := append([]scaleProof(nil), scaleProofs...)
	evidenceMu.Unlock()

	var b strings.Builder

	result := "PASSED"
	if !passed {
		result = "FAILED"
	}

	fmt.Fprintf(&b, "# E2E evidence: %s\n\n", result)
	fmt.Fprintf(&b, "Run finished %s on context `%s`.\n\n", time.Now().Format("2006-01-02 15:04:05"), k8s.context)

	writeOperatorSummary(&b, checks, scales)
	writeChecks(&b, checks)
	writeScaleActions(&b, scales)

	report := b.String()

	if err := os.WriteFile(reportPath(), []byte(report), 0o600); err != nil {
		t.Logf("could not write the evidence report: %v", err)
	} else {
		progress("evidence report written to %s", reportPath())
	}

	fmt.Fprintln(os.Stderr, report)
}

// writeOperatorSummary writes one row per operator: checks passed and the
// scale actions the downscaler logged for it.
func writeOperatorSummary(b *strings.Builder, checks []checkProof, scales []scaleProof) {
	type tally struct {
		passed, failed int
		actions        map[string]int
	}

	tallies := map[string]*tally{}
	get := func(name string) *tally {
		if tallies[name] == nil {
			tallies[name] = &tally{actions: map[string]int{}}
		}

		return tallies[name]
	}

	for _, c := range checks {
		op := operatorOf(c.check)
		if op == "" {
			continue
		}

		if c.ok {
			get(op).passed++
		} else {
			get(op).failed++
		}
	}

	for _, s := range scales {
		if op := operatorOf(s.kind + "/" + s.name); op != "" {
			get(op).actions[s.action]++
		}
	}

	b.WriteString("## By operator\n\n")
	b.WriteString("| Operator | Checks passed | Checks failed | Scale actions the downscaler logged |\n")
	b.WriteString("|---|---|---|---|\n")

	for _, op := range operators {
		tl := tallies[op.name]
		if tl == nil {
			fmt.Fprintf(b, "| %s | 0 | 0 | none: not exercised |\n", op.name)
			continue
		}

		actions := make([]string, 0, len(tl.actions))
		for action, n := range tl.actions {
			actions = append(actions, fmt.Sprintf("%s ×%d", action, n))
		}

		sort.Strings(actions)

		if len(actions) == 0 {
			actions = append(actions, "none")
		}

		fmt.Fprintf(b, "| %s | %d | %d | %s |\n", op.name, tl.passed, tl.failed, strings.Join(actions, ", "))
	}

	b.WriteString("\n")
}

// writeChecks writes every check, grouped by the subtest that ran it, in order.
func writeChecks(b *strings.Builder, checks []checkProof) {
	b.WriteString("## Checks\n")

	current := ""

	for _, c := range checks {
		if c.test != current {
			current = c.test
			fmt.Fprintf(b, "\n### %s\n\n| | Check | Saw | After |\n|---|---|---|---|\n", current)
		}

		mark := "✅"
		if !c.ok {
			mark = "❌"
		}

		took := "-"
		if c.took > 0 {
			took = c.took.String()
		}

		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", mark, escapeCell(c.check), escapeCell(c.seen), took)
	}

	b.WriteString("\n")
}

// writeScaleActions writes the downscaler's own log of what it scaled.
func writeScaleActions(b *strings.Builder, scales []scaleProof) {
	b.WriteString("## What the downscaler did\n\n")

	if len(scales) == 0 {
		b.WriteString("No scale actions were logged.\n")
		return
	}

	b.WriteString("| Time | Action | Workload | Change | Decided by |\n|---|---|---|---|---|\n")

	for _, s := range scales {
		fmt.Fprintf(b, "| %s | %s | %s/%s | %s: %s → %s | %s |\n",
			s.at.Local().Format("15:04:05"), s.action, s.kind, s.name, s.attribute, orDash(s.from), orDash(s.to), s.scope)
	}
}

func escapeCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}
