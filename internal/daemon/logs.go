package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const (
	logStoreFile         = "daemon_logs.jsonl"
	defaultLogStoreLimit = 20000
	defaultLogQueryLimit = 200
	maxLogQueryLimit     = 1000
)

// LogStore persists recent structured log entries for the /logs endpoint.
type LogStore struct {
	path       string
	maxEntries int
	compactAt  int

	mu          sync.RWMutex
	entries     []domain.LogEntry
	fileEntries int
	version     uint64
}

type logQuery struct {
	Limit     int
	Since     time.Time
	Until     time.Time
	Level     string
	Component string
	EventType string
	Action    string
	Repo      string
	Workflow  string
	Job       string
	Runner    string
	Commit    string
	Branch    string
	Search    string
}

type logResponse struct {
	Count   int               `json:"count"`
	Entries []domain.LogEntry `json:"entries"`
}

// NewLogStore creates or loads the persisted log store under the daemon state dir.
func NewLogStore(stateDir string) (*LogStore, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log store dir %s: %w", stateDir, err)
	}

	store := &LogStore{
		path:       filepath.Join(stateDir, logStoreFile),
		maxEntries: defaultLogStoreLimit,
		compactAt:  defaultLogStoreLimit * 2,
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *LogStore) load() error {
	file, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening log store %s: %w", s.path, err)
	}
	defer file.Close()

	var (
		entries         []domain.LogEntry
		fileEntries     int
		needsCompaction bool
	)
	reader := bufio.NewReaderSize(file, 64*1024)
	for lineNum := 1; ; lineNum++ {
		rawLine, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("reading log store %s: %w", s.path, readErr)
		}

		line := strings.TrimSpace(rawLine)
		if line == "" {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		var entry domain.LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			if errors.Is(readErr, io.EOF) && isTruncatedLogEntry(line, err) {
				needsCompaction = true
				break
			}
			return fmt.Errorf("decoding log store %s line %d: %w", s.path, lineNum, err)
		}
		entry.Time = entry.Time.UTC()
		if entry.Attributes != nil && len(entry.Attributes) == 0 {
			entry.Attributes = nil
		}
		entries = append(entries, entry)
		fileEntries++
		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	if len(entries) > s.maxEntries {
		entries = append([]domain.LogEntry(nil), entries[len(entries)-s.maxEntries:]...)
		needsCompaction = true
	}

	s.mu.Lock()
	s.entries = entries
	s.fileEntries = fileEntries
	s.mu.Unlock()

	if needsCompaction {
		s.mu.Lock()
		err := s.compactLocked()
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func isTruncatedLogEntry(line string, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "unexpected end of JSON input") || strings.Contains(msg, "unexpected EOF") {
		return true
	}

	switch trailingLiteralFragment(line) {
	case "t", "tr", "tru", "f", "fa", "fal", "fals", "n", "nu", "nul":
		return true
	default:
		return false
	}
}

func trailingLiteralFragment(line string) string {
	line = strings.TrimRightFunc(line, unicode.IsSpace)
	end := len(line)
	start := end
	for start > 0 {
		r := rune(line[start-1])
		if !unicode.IsLetter(r) {
			break
		}
		start--
	}
	return line[start:end]
}

// Record appends a new log entry and persists the bounded buffer to disk.
func (s *LogStore) Record(entry domain.LogEntry) error {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	} else {
		entry.Time = entry.Time.UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	line, err := encodeLogEntry(entry)
	if err != nil {
		return err
	}
	if err := s.appendLocked(line); err != nil {
		return err
	}

	s.entries = append(s.entries, entry)
	s.fileEntries++
	s.version++

	trimmed := false
	if len(s.entries) > s.maxEntries {
		s.entries = append([]domain.LogEntry(nil), s.entries[len(s.entries)-s.maxEntries:]...)
		trimmed = true
	}
	if trimmed || (s.compactAt > 0 && s.fileEntries > s.compactAt) {
		return s.compactLocked()
	}
	return nil
}

// Query returns the newest matching log entries first.
func (s *LogStore) Query(query logQuery) []domain.LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := normalizeLimit(query.Limit)
	results := make([]domain.LogEntry, 0, limit)
	for i := len(s.entries) - 1; i >= 0; i-- {
		entry := s.entries[i]
		if !matchesLogQuery(entry, query) {
			continue
		}
		results = append(results, entry)
		if len(results) == limit {
			break
		}
	}
	return results
}

// Snapshot returns a stable copy of all retained log entries in chronological order.
func (s *LogStore) Snapshot() []domain.LogEntry {
	_, entries := s.SnapshotWithVersion()
	return entries
}

// Version returns the current mutation version for the retained log buffer.
func (s *LogStore) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// SnapshotWithVersion returns a stable copy of all retained log entries plus the current mutation version.
func (s *LogStore) SnapshotWithVersion() (uint64, []domain.LogEntry) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.entries) == 0 {
		return s.version, nil
	}
	entries := make([]domain.LogEntry, len(s.entries))
	copy(entries, s.entries)
	return s.version, entries
}

func encodeLogEntry(entry domain.LogEntry) ([]byte, error) {
	payload, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("encoding log entry: %w", err)
	}
	return append(payload, '\n'), nil
}

func (s *LogStore) appendLocked(line []byte) error {
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log store for append: %w", err)
	}
	if _, err := file.Write(line); err != nil {
		file.Close()
		return fmt.Errorf("appending log entry: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing appended log store file: %w", err)
	}
	return nil
}

func (s *LogStore) compactLocked() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "daemon-logs-*.tmp")
	if err != nil {
		return fmt.Errorf("creating log store temp file: %w", err)
	}

	encoder := json.NewEncoder(tmp)
	for _, entry := range s.entries {
		if err := encoder.Encode(entry); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("encoding log entry: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("closing log store temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("renaming log store temp file: %w", err)
	}

	s.fileEntries = len(s.entries)
	return nil
}

type logStoreHandler struct {
	next   slog.Handler
	store  *LogStore
	attrs  []slog.Attr
	groups []string
}

// NewLogHandler mirrors standard slog output to the persistent log store.
func NewLogHandler(next slog.Handler, store *LogStore) slog.Handler {
	return &logStoreHandler{
		next:  next,
		store: store,
	}
}

func (h *logStoreHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *logStoreHandler) Handle(ctx context.Context, record slog.Record) error {
	cloned := record.Clone()
	err := h.next.Handle(ctx, record)
	if h.store == nil {
		return err
	}
	if storeErr := h.store.Record(buildLogEntry(cloned, h.attrs, h.groups)); storeErr != nil && err == nil {
		return storeErr
	}
	return err
}

func (h *logStoreHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logStoreHandler{
		next:   h.next.WithAttrs(attrs),
		store:  h.store,
		attrs:  append(copyAttrs(h.attrs), attrs...),
		groups: append([]string(nil), h.groups...),
	}
}

func (h *logStoreHandler) WithGroup(name string) slog.Handler {
	return &logStoreHandler{
		next:   h.next.WithGroup(name),
		store:  h.store,
		attrs:  copyAttrs(h.attrs),
		groups: append(append([]string(nil), h.groups...), name),
	}
}

func copyAttrs(attrs []slog.Attr) []slog.Attr {
	if len(attrs) == 0 {
		return nil
	}
	cloned := make([]slog.Attr, len(attrs))
	copy(cloned, attrs)
	return cloned
}

func buildLogEntry(record slog.Record, baseAttrs []slog.Attr, groups []string) domain.LogEntry {
	entry := domain.LogEntry{
		Time:    record.Time.UTC(),
		Level:   record.Level.String(),
		Message: record.Message,
	}

	for _, attr := range baseAttrs {
		applyLogAttr(&entry, groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		applyLogAttr(&entry, groups, attr)
		return true
	})

	if entry.Runner == "" && entry.Container != "" {
		entry.Runner = entry.Container
	}
	if len(entry.Attributes) == 0 {
		entry.Attributes = nil
	}
	return entry
}

func applyLogAttr(entry *domain.LogEntry, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}

	keyParts := append([]string(nil), groups...)
	if attr.Key != "" {
		keyParts = append(keyParts, attr.Key)
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, nested := range attr.Value.Group() {
			applyLogAttr(entry, keyParts, nested)
		}
		return
	}

	key := strings.Join(keyParts, ".")
	setLogField(entry, key, slogValueString(attr.Value))
}

func setLogField(entry *domain.LogEntry, key, value string) {
	switch key {
	case "component":
		entry.Component = value
	case "event_type":
		entry.EventType = value
	case "action":
		entry.Action = value
	case "repo":
		entry.Repo = value
	case "workflow":
		entry.Workflow = value
	case "job":
		entry.Job = value
	case "job_id":
		entry.JobID = parseLogInt(value)
	case "runner":
		entry.Runner = value
	case "container":
		entry.Container = value
	case "commit":
		entry.Commit = value
	case "branch":
		entry.Branch = value
	case "status":
		entry.Status = value
	case "conclusion":
		entry.Conclusion = value
	case "detail":
		entry.Detail = value
	case "cache_path":
		entry.CachePath = value
	case "run_id":
		entry.RunID = parseLogInt(value)
	case "run_attempt":
		entry.RunAttempt = int(parseLogInt(value))
	case "error":
		entry.Error = value
	default:
		if entry.Attributes == nil {
			entry.Attributes = make(map[string]string)
		}
		entry.Attributes[key] = value
	}
}

func slogValueString(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'f', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(value.Any())
	default:
		return value.String()
	}
}

func parseLogInt(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseLogQuery(values map[string][]string) (logQuery, error) {
	query := logQuery{
		Level:     firstQueryValue(values, "level"),
		Component: firstQueryValue(values, "component"),
		EventType: firstQueryValue(values, "event_type"),
		Action:    firstQueryValue(values, "action"),
		Repo:      firstQueryValue(values, "repo"),
		Workflow:  firstQueryValue(values, "workflow"),
		Job:       firstQueryValue(values, "job"),
		Runner:    firstQueryValue(values, "runner"),
		Commit:    firstQueryValue(values, "commit"),
		Branch:    firstQueryValue(values, "branch"),
		Search:    firstQueryValue(values, "q"),
	}

	if limitText := firstQueryValue(values, "limit"); limitText != "" {
		limit, err := strconv.Atoi(limitText)
		if err != nil {
			return logQuery{}, fmt.Errorf("invalid limit %q", limitText)
		}
		query.Limit = limit
	}

	since, err := parseOptionalTime(firstQueryValue(values, "since"))
	if err != nil {
		return logQuery{}, err
	}
	until, err := parseOptionalTime(firstQueryValue(values, "until"))
	if err != nil {
		return logQuery{}, err
	}
	query.Since = since
	query.Until = until

	return query, nil
}

func firstQueryValue(values map[string][]string, key string) string {
	if len(values[key]) == 0 {
		return ""
	}
	return strings.TrimSpace(values[key][0])
}

func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q, expected RFC3339", value)
	}
	return parsed.UTC(), nil
}

func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLogQueryLimit
	case limit > maxLogQueryLimit:
		return maxLogQueryLimit
	default:
		return limit
	}
}

func matchesLogQuery(entry domain.LogEntry, query logQuery) bool {
	if !query.Since.IsZero() && entry.Time.Before(query.Since) {
		return false
	}
	if !query.Until.IsZero() && entry.Time.After(query.Until) {
		return false
	}
	if query.Level != "" && !strings.EqualFold(entry.Level, query.Level) {
		return false
	}
	if query.Component != "" && !strings.EqualFold(entry.Component, query.Component) {
		return false
	}
	if query.EventType != "" && !strings.EqualFold(entry.EventType, query.EventType) {
		return false
	}
	if query.Action != "" && !strings.EqualFold(entry.Action, query.Action) {
		return false
	}
	if query.Repo != "" && !strings.EqualFold(entry.Repo, query.Repo) {
		return false
	}
	if query.Workflow != "" && !containsFold(entry.Workflow, query.Workflow) {
		return false
	}
	if query.Job != "" && !containsFold(entry.Job, query.Job) {
		return false
	}
	if query.Runner != "" && !strings.EqualFold(entry.Runner, query.Runner) && !strings.EqualFold(entry.Container, query.Runner) {
		return false
	}
	if query.Commit != "" && !strings.HasPrefix(strings.ToLower(entry.Commit), strings.ToLower(query.Commit)) {
		return false
	}
	if query.Branch != "" && !strings.EqualFold(entry.Branch, query.Branch) {
		return false
	}
	if query.Search != "" && !logEntryContains(entry, query.Search) {
		return false
	}
	return true
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func logEntryContains(entry domain.LogEntry, query string) bool {
	fields := []string{
		entry.Message,
		entry.Detail,
		entry.Repo,
		entry.Workflow,
		entry.Job,
		entry.Runner,
		entry.Container,
		entry.Commit,
		entry.Branch,
		entry.Error,
	}
	for _, field := range fields {
		if containsFold(field, query) {
			return true
		}
	}
	for key, value := range entry.Attributes {
		if containsFold(key, query) || containsFold(value, query) {
			return true
		}
	}
	return false
}
