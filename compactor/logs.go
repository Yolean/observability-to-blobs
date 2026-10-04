package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// The logs API (compactor/README.md, "The logs API"): container logs by URL,
// with the paths, query parameters, response headers and errors of an
// existing logs server, so that its clients switch to this one unchanged
// for retrieval. The operations are registered from typed Go input and
// output structs, and huma generates the OpenAPI 3.1 spec from them,
// served at /openapi.json with docs at /docs and committed as
// compactor/openapi.json (`blobs-compactor openapi`); clients generate
// their types from it, so the spec is part of the contract, and
// openapi_test.go holds it to the reference's.

// --- Typed enums: consts for compile-time safety, Schema() for OpenAPI enum generation ---

// TailEffect indicates whether the tail parameter truncated the output.
type TailEffect string

const (
	TailEffectCap     TailEffect = "cap"
	TailEffectAll     TailEffect = "all"
	TailEffectUnknown TailEffect = "unknown"
)

func (TailEffect) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{Type: "string", Enum: []any{TailEffectCap, TailEffectAll, TailEffectUnknown}}
}

// PodPhase is the Kubernetes pod phase.
type PodPhase string

const (
	PodPhaseRunning   PodPhase = "Running"
	PodPhaseSucceeded PodPhase = "Succeeded"
	PodPhaseFailed    PodPhase = "Failed"
	PodPhasePending   PodPhase = "Pending"
	PodPhaseUnknown   PodPhase = "Unknown"
	PodPhaseArchived  PodPhase = "Archived"
)

func (PodPhase) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{Type: "string", Enum: []any{PodPhaseRunning, PodPhaseSucceeded, PodPhaseFailed, PodPhasePending, PodPhaseUnknown, PodPhaseArchived}}
}

// LogsSource indicates whether logs were served from K8s, blob storage, or
// both. This server has blob storage only.
type LogsSource string

const (
	LogsSourceK8s     LogsSource = "k8s"
	LogsSourceBlob    LogsSource = "blob"
	LogsSourceBlobK8s LogsSource = "blob+k8s"
)

func (LogsSource) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{Type: "string", Enum: []any{LogsSourceK8s, LogsSourceBlob, LogsSourceBlobK8s}}
}

// CRITimestamp is a CRI log timestamp in RFC3339Nano format (e.g. "2024-01-15T10:30:00.123456789Z").
// Always UTC (Z suffix), with nanosecond fractional seconds.
type CRITimestamp string

func (CRITimestamp) Schema(r huma.Registry) *huma.Schema {
	length := 30 // 2024-01-15T10:30:00.123456789Z
	return &huma.Schema{
		Type:        "string",
		MinLength:   &length,
		MaxLength:   &length,
		Pattern:     `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z$`,
		Description: "CRI log timestamp in RFC3339Nano format, always UTC with 9-digit nanoseconds",
	}
}

// criLayout renders a line's time as a CRITimestamp: always nine digits,
// so the strings sort as the times do.
const criLayout = "2006-01-02T15:04:05.000000000Z"

// --- Input/Output types for OpenAPI spec generation ---

// LogsInput defines the path and query parameters for the logs endpoint.
type LogsInput struct {
	Namespace  string `path:"namespace" doc:"Kubernetes namespace"`
	Pod        string `path:"pod" doc:"Pod name"`
	Container  string `path:"container" required:"false" doc:"Container name. Omit for all containers merged by timestamp."`
	Timestamps bool   `query:"timestamps" doc:"Include RFC3339Nano timestamps" default:"false"`
	Prefix     bool   `query:"prefix" doc:"Prefix lines with [pod/{name}/{container}]" default:"false"`
	Tail       int    `query:"tail" doc:"Max lines returned (applied after grep)" default:"100"`
	Grep       string `query:"grep" doc:"Line filter. Plain substring match, or ^-prefixed for anchored prefix match." default:""`
	Ansi       string `query:"ansi" doc:"Set to 'strip' to remove ANSI escape sequences from output." default:""`
	Source     string `query:"source" enum:"blob,k8s" doc:"Force log source. Omit for automatic selection based on pod phase." default:""`
}

// LogsAllContainersInput has no container path param.
type LogsAllContainersInput struct {
	Namespace  string `path:"namespace" doc:"Kubernetes namespace"`
	Pod        string `path:"pod" doc:"Pod name"`
	Timestamps bool   `query:"timestamps" doc:"Include RFC3339Nano timestamps" default:"false"`
	Prefix     bool   `query:"prefix" doc:"Prefix lines with [pod/{name}/{container}]" default:"false"`
	Tail       int    `query:"tail" doc:"Max lines returned (applied after grep)" default:"100"`
	Grep       string `query:"grep" doc:"Line filter. Plain substring match, or ^-prefixed for anchored prefix match." default:""`
	Ansi       string `query:"ansi" doc:"Set to 'strip' to remove ANSI escape sequences from output." default:""`
	Source     string `query:"source" enum:"blob,k8s" doc:"Force log source. Omit for automatic selection based on pod phase." default:""`
}

// LogsHeaders are response headers shared by all log retrieval endpoints.
type LogsHeaders struct {
	ContentType      string       `header:"Content-Type" default:"text/plain; charset=utf-8"`
	LogsTimestampEnd CRITimestamp `header:"X-Logs-Timestamp-End" doc:"Last CRI timestamp of the returned logs"`
	LogsPodPhase     PodPhase     `header:"X-Logs-Pod-Phase" doc:"Pod phase from cluster index"`
	LogsTailEffect   TailEffect   `header:"X-Logs-Tail-Effect" doc:"Whether tail truncated the output"`
	LogsSource       LogsSource   `header:"X-Logs-Source" doc:"Source of logs: k8s or blob"`
}

// LogsOutput is plain text log output.
type LogsOutput struct {
	LogsHeaders
	Body []byte
}

// textPlain is the Content-Type of every logs answer: set, not sniffed,
// so that no line can make the body another type.
const textPlain = "text/plain; charset=utf-8"

// newLogsAPI creates the huma API on mux: the OpenAPI 3.1 spec at
// /openapi.json, docs at /docs, errors as RFC 9457 problem JSON.
func newLogsAPI(mux *http.ServeMux) huma.API {
	config := huma.DefaultConfig("blobs-compactor", "1.0.0")
	config.Info.Description = "Kubernetes container logs from the hourly and daily tiers"
	return humago.New(mux, config)
}

// logsHandler serves the logs operations from a reader. Each request has
// a deadline (LOGS_TIMEOUT) and a slot of LOGS_CONCURRENCY.
type logsHandler struct {
	r       *logsReader
	timeout time.Duration
	slots   chan struct{}
	log     *slog.Logger
}

func newLogsHandler(r *logsReader, timeout time.Duration, concurrency int, log *slog.Logger) *logsHandler {
	return &logsHandler{r: r, timeout: timeout, slots: make(chan struct{}, concurrency), log: log}
}

// registerLogs registers the log retrieval operations.
func registerLogs(api huma.API, h *logsHandler) {
	huma.Register(api, huma.Operation{
		OperationID: "get-pod-logs-container",
		Method:      http.MethodGet,
		Path:        "/api/v1/logs/n/{namespace}/{pod}/{container}",
		Summary:     "Get pod logs for a specific container",
		Description: "Returns plain text logs of the pod's latest incarnation from the hourly and daily tiers. Timestamps are stripped by default.",
		Tags:        []string{"Logs"},
	}, h.handleLogs)

	huma.Register(api, huma.Operation{
		OperationID: "get-pod-logs",
		Method:      http.MethodGet,
		Path:        "/api/v1/logs/n/{namespace}/{pod}",
		Summary:     "Get pod logs for all containers",
		Description: "Returns merged logs from all containers sorted by timestamp.",
		Tags:        []string{"Logs"},
	}, h.handleLogsAllContainers)
}

// openAPISpec is the spec the API serves, as `blobs-compactor openapi`
// prints it and compactor/openapi.json holds it.
func openAPISpec() ([]byte, error) {
	api := newLogsAPI(http.NewServeMux())
	registerLogs(api, &logsHandler{})
	b, err := json.MarshalIndent(api.OpenAPI(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (h *logsHandler) handleLogs(ctx context.Context, input *LogsInput) (*LogsOutput, error) {
	return h.getLogs(ctx, input.Namespace, input.Pod, input.Container, input.Timestamps, input.Prefix, input.Tail, input.Grep, input.Ansi == "strip", input.Source)
}

func (h *logsHandler) handleLogsAllContainers(ctx context.Context, input *LogsAllContainersInput) (*LogsOutput, error) {
	return h.getLogs(ctx, input.Namespace, input.Pod, "", input.Timestamps, input.Prefix, input.Tail, input.Grep, input.Ansi == "strip", input.Source)
}

// getLogs is the shared implementation of both operations. source may be
// "" or "blob"; this server has no live source, so "k8s" is a 400, and so
// is any other value (which huma's enum check answers with a 422 first).
func (h *logsHandler) getLogs(ctx context.Context, ns, pod, container string, showTimestamps, showPrefix bool, tail int, grep string, stripAnsi bool, source string) (*LogsOutput, error) {
	switch source {
	case "", string(LogsSourceBlob):
	case string(LogsSourceK8s):
		return nil, huma.Error400BadRequest("invalid source value: this server has no k8s source, use 'blob' or omit it")
	default:
		return nil, huma.Error400BadRequest("invalid source value: must be 'blob' or 'k8s'")
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-ctx.Done():
		return nil, huma.Error503ServiceUnavailable(fmt.Sprintf("no log request slot within %s: LOGS_CONCURRENCY requests are in flight", h.timeout))
	}
	inc, lines, err := h.r.read(ctx, ns, pod, lineQuery{container: container, grep: grep, stripAnsi: stripAnsi, tail: tail})
	var nc *errNoContainer
	var se *errStorage
	switch {
	case errors.Is(err, errNoPod):
		return nil, huma.Error404NotFound(fmt.Sprintf("no archived logs for %s/%s", ns, pod))
	case errors.As(err, &nc):
		return nil, huma.Error404NotFound(fmt.Sprintf(
			"container %q not found in pod %s/%s, known containers: [%s]",
			container, ns, pod, strings.Join(nc.known, ", ")))
	case errors.As(err, &se):
		h.log.Error("logs", "namespace", ns, "pod", pod, "container", container, "err", err)
		if se.op == "list" {
			return nil, huma.Error500InternalServerError("blob list failed: " + se.err.Error())
		}
		return nil, huma.Error500InternalServerError("blob read failed: " + se.err.Error())
	case err != nil:
		h.log.Error("logs", "namespace", ns, "pod", pod, "container", container, "err", err)
		return nil, huma.Error500InternalServerError("blob read failed: " + err.Error())
	}
	all := make([]timestampedLine, len(lines))
	for i, l := range lines {
		all[i] = timestampedLine{timestamp: l.time.UTC().Format(criLayout), container: l.container, message: l.message}
	}
	return formatOutput(all, pod, showTimestamps, showPrefix, tail, grep, stripAnsi, inc.phase, LogsSourceBlob), nil
}

// timestampedLine holds a log line with its timestamp for sorting.
type timestampedLine struct {
	timestamp string
	container string
	message   string
}

// matchGrep returns true if the line message matches the grep filter.
// Empty grep matches everything. ^-prefixed patterns are anchored prefix matches.
// All other patterns are plain substring matches.
func matchGrep(message, grep string) bool {
	if grep == "" {
		return true
	}
	if strings.HasPrefix(grep, "^") {
		return strings.HasPrefix(message, grep[1:])
	}
	return strings.Contains(message, grep)
}

// formatOutput applies sorting, ANSI stripping, grep, tail, and builds the
// text response, as the reference does. The reader has applied the same
// filters in the engine already; here they are exact.
func formatOutput(allLines []timestampedLine, pod string, showTimestamps, showPrefix bool, tail int, grep string, stripAnsi bool, podPhase PodPhase, source LogsSource) *LogsOutput {
	if len(allLines) > 1 {
		sort.SliceStable(allLines, func(i, j int) bool {
			return allLines[i].timestamp < allLines[j].timestamp
		})
	}

	// Strip ANSI escape sequences before grep so filters match clean text
	if stripAnsi {
		for i := range allLines {
			allLines[i].message = removeAnsi(allLines[i].message)
		}
	}

	if grep != "" {
		filtered := make([]timestampedLine, 0, len(allLines))
		for _, line := range allLines {
			if matchGrep(line.message, grep) {
				filtered = append(filtered, line)
			}
		}
		allLines = filtered
	}

	// Determine tail effect before truncating
	tailEffect := TailEffectAll
	if tail > 0 && len(allLines) > tail {
		allLines = allLines[len(allLines)-tail:]
		tailEffect = TailEffectCap
	}

	var buf strings.Builder
	var lastTimestamp CRITimestamp
	for _, line := range allLines {
		if line.timestamp != "" {
			lastTimestamp = CRITimestamp(line.timestamp)
		}
		var parts []string
		if showPrefix {
			parts = append(parts, fmt.Sprintf("[pod/%s/%s]", pod, line.container))
		}
		if showTimestamps && line.timestamp != "" {
			parts = append(parts, line.timestamp)
		}
		parts = append(parts, line.message)
		buf.WriteString(strings.Join(parts, " "))
		buf.WriteByte('\n')
	}

	return &LogsOutput{
		Body: []byte(buf.String()),
		LogsHeaders: LogsHeaders{
			ContentType:      textPlain,
			LogsTimestampEnd: lastTimestamp,
			LogsPodPhase:     podPhase,
			LogsTailEffect:   tailEffect,
			LogsSource:       source,
		},
	}
}

// removeAnsi strips ANSI escape sequences (CSI sequences like \x1b[...m) from s.
// Scans byte-by-byte with no buffering beyond the output builder.
func removeAnsi(s string) string {
	if strings.IndexByte(s, '\x1b') < 0 {
		return s
	}
	var buf strings.Builder
	buf.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// Skip CSI sequence: ESC [ ... final byte
			j := i + 2
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3F {
				j++ // parameter bytes and intermediate bytes
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7E {
				j++ // final byte
			}
			i = j
		} else {
			buf.WriteByte(s[i])
			i++
		}
	}
	return buf.String()
}

// logsServer is the logs API on addr.
func logsServer(addr string, h *logsHandler) *http.Server {
	mux := http.NewServeMux()
	registerLogs(newLogsAPI(mux), h)
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: h.timeout + time.Minute, IdleTimeout: 2 * time.Minute}
}
