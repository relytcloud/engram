package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/internal/diagnostic"
)

// mem_doctor's registered diagnostic.Runner checks all require direct SQL
// access via a concrete *store.Store (diagnostic.Scope.Store) — something a
// MemoryLake-backed project's MemoryBackend cannot provide. Rather than
// hard-erroring for those projects, mem_doctor substitutes this lightweight
// suite, built entirely on the MemoryBackend interface: a connectivity+auth
// round trip (ProjectExists, which hits MemoryLake's authenticated project
// list endpoint) and a data-availability round trip (Stats), each reporting
// its own latency. This is deliberately a reduced check set, not a rewrite of
// diagnostic.Runner's SQL-specific checks (session/directory drift, sync
// mutation field validation, SQLite lock contention) — those genuinely don't
// have a MemoryLake analogue yet.

const (
	// CheckMemoryLakeConnectivity verifies the MemoryLake backend is
	// reachable and authenticated by asking it whether the current project
	// exists (a real authenticated network round trip).
	CheckMemoryLakeConnectivity = "memorylake_connectivity"
	// CheckMemoryLakeStats verifies the MemoryLake backend can serve
	// aggregate stats for the current project (a second, independent round
	// trip exercising the read path mem_stats depends on).
	CheckMemoryLakeStats = "memorylake_stats"
)

// memoryLakeDoctorChecks lists the check IDs the lightweight suite knows how
// to run, in run order (used for the "no --check given" full-suite case).
var memoryLakeDoctorChecks = []string{CheckMemoryLakeConnectivity, CheckMemoryLakeStats}

// runMemoryLakeDoctor builds a mem_doctor diagnostic.Report for a project
// whose resolved backend is not a concrete *store.Store, using only the
// MemoryBackend interface. It never returns an error: any failure reaching
// the backend is captured as a StatusError diagnostic.CheckResult inside the
// returned report (fail-safe), matching the contract handleDoctor already
// expects from diagnostic.Runner.
func runMemoryLakeDoctor(ctx context.Context, s MemoryBackend, project string, check string) diagnostic.Report {
	_ = ctx // no MemoryBackend method here takes a context; kept for signature symmetry with diagnostic.Runner
	check = strings.TrimSpace(check)

	if check != "" && !memoryLakeChecksContain(check) {
		return buildMemoryLakeReport(project, []diagnostic.CheckResult{memoryLakeCheckNotApplicable(check)})
	}

	ids := memoryLakeDoctorChecks
	if check != "" {
		ids = []string{check}
	}

	results := make([]diagnostic.CheckResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, runMemoryLakeCheck(s, project, id))
	}
	return buildMemoryLakeReport(project, results)
}

func memoryLakeChecksContain(check string) bool {
	for _, id := range memoryLakeDoctorChecks {
		if id == check {
			return true
		}
	}
	return false
}

func runMemoryLakeCheck(s MemoryBackend, project string, id string) diagnostic.CheckResult {
	switch id {
	case CheckMemoryLakeConnectivity:
		return memoryLakeConnectivityCheck(s, project)
	case CheckMemoryLakeStats:
		return memoryLakeStatsCheck(s)
	default:
		return memoryLakeCheckNotApplicable(id)
	}
}

func memoryLakeConnectivityCheck(s MemoryBackend, project string) diagnostic.CheckResult {
	start := time.Now()
	exists, err := s.ProjectExists(project)
	elapsed := time.Since(start)
	if err != nil {
		return diagnostic.CheckResult{
			CheckID:      CheckMemoryLakeConnectivity,
			Result:       diagnostic.StatusError,
			Severity:     diagnostic.SeverityError,
			ReasonCode:   "memorylake_unreachable",
			Message:      fmt.Sprintf("MemoryLake backend did not respond after %s: %s", elapsed, err.Error()),
			Why:          "mem_doctor could not verify MemoryLake connectivity/authentication for this project.",
			Evidence:     memoryLakeJSON(map[string]any{"latency_ms": elapsed.Milliseconds(), "error": err.Error()}),
			SafeNextStep: "Check MemoryLake credentials and network reachability (ENGRAM_MEMORYLAKE_* config), then rerun mem_doctor.",
		}
	}
	return diagnostic.CheckResult{
		CheckID:      CheckMemoryLakeConnectivity,
		Result:       diagnostic.StatusOK,
		Severity:     diagnostic.SeverityInfo,
		ReasonCode:   "memorylake_connectivity_ok",
		Message:      fmt.Sprintf("MemoryLake backend responded in %s (project_exists=%t).", elapsed, exists),
		Why:          "Verifies the MemoryLake-backed project's endpoint is reachable, authenticated, and knows this project.",
		Evidence:     memoryLakeJSON(map[string]any{"latency_ms": elapsed.Milliseconds(), "project_exists": exists}),
		SafeNextStep: "None — connectivity looks healthy.",
	}
}

func memoryLakeStatsCheck(s MemoryBackend) diagnostic.CheckResult {
	start := time.Now()
	stats, err := s.Stats()
	elapsed := time.Since(start)
	if err != nil {
		return diagnostic.CheckResult{
			CheckID:      CheckMemoryLakeStats,
			Result:       diagnostic.StatusError,
			Severity:     diagnostic.SeverityError,
			ReasonCode:   "memorylake_stats_unavailable",
			Message:      fmt.Sprintf("MemoryLake backend could not serve stats after %s: %s", elapsed, err.Error()),
			Why:          "mem_stats depends on the same read path; a failure here means mem_stats will also fail for this project.",
			Evidence:     memoryLakeJSON(map[string]any{"latency_ms": elapsed.Milliseconds(), "error": err.Error()}),
			SafeNextStep: "Check MemoryLake credentials and network reachability, then rerun mem_doctor or mem_stats.",
		}
	}
	return diagnostic.CheckResult{
		CheckID:      CheckMemoryLakeStats,
		Result:       diagnostic.StatusOK,
		Severity:     diagnostic.SeverityInfo,
		ReasonCode:   "memorylake_stats_ok",
		Message:      fmt.Sprintf("MemoryLake backend served stats in %s (%d observations).", elapsed, stats.TotalObservations),
		Why:          "Verifies the read path mem_stats depends on for this project.",
		Evidence:     memoryLakeJSON(map[string]any{"latency_ms": elapsed.Milliseconds(), "total_observations": stats.TotalObservations}),
		SafeNextStep: "None — stats look healthy.",
	}
}

// memoryLakeCheckNotApplicable is returned when mem_doctor is asked to run a
// specific --check that belongs to the SQLite-only diagnostic.Runner suite
// (or an unrecognized id) against a MemoryLake-backed project. This is
// intentionally an OK/info result, not an error: the check simply does not
// apply to this backend, which is expected and not a health problem.
func memoryLakeCheckNotApplicable(check string) diagnostic.CheckResult {
	return diagnostic.CheckResult{
		CheckID:    check,
		Result:     diagnostic.StatusOK,
		Severity:   diagnostic.SeverityInfo,
		ReasonCode: "check_not_applicable_memorylake",
		Message:    fmt.Sprintf("Check %q requires the local SQLite backend and does not apply to MemoryLake-backed projects.", check),
		Why:        "This diagnostic runs direct SQL against the local store; MemoryLake-backed projects have no local SQLite row to check.",
		Evidence:   memoryLakeJSON(map[string]any{"requested_check": check, "available_checks": memoryLakeDoctorChecks}),
		SafeNextStep: fmt.Sprintf(
			"Run mem_doctor without --check for the MemoryLake connectivity/stats suite (%s), or against a SQLite-backed project for the full check.",
			strings.Join(memoryLakeDoctorChecks, ", "),
		),
	}
}

// buildMemoryLakeReport aggregates check results into a diagnostic.Report
// using the same status/summary rules as diagnostic package's (unexported)
// report builder, so mem_doctor's response envelope shape is identical
// whether the checks came from diagnostic.Runner or this MemoryLake-lite
// suite.
func buildMemoryLakeReport(project string, checks []diagnostic.CheckResult) diagnostic.Report {
	report := diagnostic.Report{Status: diagnostic.StatusOK, Project: project, Checks: checks}
	report.Summary.Total = len(checks)
	for _, c := range checks {
		switch c.Result {
		case diagnostic.StatusError:
			report.Summary.Errors++
		case diagnostic.StatusBlocked:
			report.Summary.Blocked++
		case diagnostic.StatusWarning:
			report.Summary.Warnings++
		default:
			report.Summary.OK++
		}
	}
	switch {
	case report.Summary.Errors > 0:
		report.Status = diagnostic.StatusError
	case report.Summary.Blocked > 0:
		report.Status = diagnostic.StatusBlocked
	case report.Summary.Warnings > 0:
		report.Status = diagnostic.StatusWarning
	default:
		report.Status = diagnostic.StatusOK
	}
	return report
}

func memoryLakeJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
