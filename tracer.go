package tracer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultGroupCount   = 40 // total groups
	DefaultSpanCount    = 60 // total spans per group
	DefaultMessageCount = 60 // total messages per span
)

type Tracer interface {
	Trace(group, span string) Logger
	Group(group string) Logger

	ListGroups() []string
	ListSpans(group string) []string

	Logs(group string) [][]LogEntry
	ToMap(timezone string, withExactTime bool, groupFilter, spanFilter string) (map[string]map[string][]string, []byte)

	Enable()  // by default tracer is enabled
	Disable() // disable all logging, turning each call into a noop
	IsEnabled() bool
}

type Logger interface {
	Span(span string) Logger
	With(group, span string) Logger

	GetGroup() string
	GetSpan() string

	Info(message string, v ...any)
	Warn(message string, v ...any)
	Error(message string, v ...any)
}

type LogEntry interface {
	Level() string
	Group() string
	Span() string
	Message() string
	Time() time.Time
	TimeAgo(timezone ...string) string
	Count() uint32
	FormattedMessage(timezone string, withExactTime ...bool) string
}

type tracer struct {
	logs                             map[string]map[string][]logEntry
	numGroups, numSpans, numMessages int
	enabled                          bool
	groupTS                          map[string]time.Time
	spanTS                           map[string]map[string]time.Time
	mu                               sync.RWMutex
}

func NewTracer() Tracer {
	return NewTracerWithSizes(DefaultGroupCount, DefaultSpanCount, DefaultMessageCount)
}

func NewTracerWithSizes(numGroups, numSpans, numMessages int) Tracer {
	if numGroups < 1 {
		numGroups = DefaultGroupCount
	}
	if numSpans < 1 {
		numSpans = DefaultSpanCount
	}
	if numMessages < 1 {
		numMessages = DefaultMessageCount
	}

	return &tracer{
		logs:        make(map[string]map[string][]logEntry),
		numGroups:   numGroups,
		numSpans:    numSpans,
		numMessages: numMessages,
		enabled:     true,
		groupTS:     make(map[string]time.Time),
		spanTS:      make(map[string]map[string]time.Time),
	}
}

func Noop() Tracer {
	tracer := NewTracerWithSizes(1, 1, 1)
	tracer.Disable()
	return tracer
}

func (t *tracer) Trace(group, span string) Logger {
	return &logger{
		tracer: t,
		group:  group,
		span:   span,
	}
}

func (t *tracer) Group(group string) Logger {
	return &logger{
		tracer: t,
		group:  group,
	}
}

func (t *tracer) ListGroups() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	groups := make([]string, 0, len(t.logs))
	for group := range t.logs {
		groups = append(groups, group)
	}
	return groups
}

func (t *tracer) ListSpans(group string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	spans := make([]string, 0, len(t.logs[group]))
	for span := range t.logs[group] {
		spans = append(spans, span)
	}
	return spans
}

func (t *tracer) Logs(group string) [][]LogEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if _, ok := t.logs[group]; !ok {
		return [][]LogEntry{}
	}

	spans := make([]string, 0, len(t.logs[group]))
	for span := range t.logs[group] {
		spans = append(spans, span)
	}

	sort.Slice(spans, func(i, j int) bool {
		timeI := t.spanTS[group][spans[i]]
		timeJ := t.spanTS[group][spans[j]]
		return timeI.After(timeJ) // most recent first
	})

	out := make([][]LogEntry, 0, len(spans))
	for _, span := range spans {
		entries := t.logs[group][span]
		outSpan := make([]LogEntry, 0, len(entries))
		for _, entry := range entries {
			outSpan = append(outSpan, entry)
		}
		sort.Slice(outSpan, func(i, j int) bool {
			return outSpan[i].Time().After(outSpan[j].Time())
		})
		out = append(out, outSpan)
	}

	return out
}

func (t *tracer) ToMap(timezone string, withExactTime bool, groupFilter, spanFilter string) (map[string]map[string][]string, []byte) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var m = make(map[string]map[string][]string)
	var jsonBuf bytes.Buffer

	// custom json output to ensure desired ordering of map keys
	jsonBuf.WriteString(`{`)

	groups := make([]string, 0, len(t.logs))
	for group := range t.logs {
		if groupFilter != "" && !strings.HasPrefix(group, groupFilter) {
			continue
		}
		groups = append(groups, group)
	}

	sort.Slice(groups, func(i, j int) bool {
		timeI := t.groupTS[groups[i]]
		timeJ := t.groupTS[groups[j]]
		return timeI.After(timeJ) // most recent first
	})

	for i, group := range groups {
		if i > 0 {
			jsonBuf.WriteString(`,`)
		}
		v, _ := json.Marshal(group)
		jsonBuf.WriteString(fmt.Sprintf(`%s:{`, v))

		spans := t.logs[group]

		spanNames := make([]string, 0, len(spans))
		for span := range spans {
			if spanFilter != "" && !strings.HasPrefix(span, spanFilter) {
				continue
			}
			spanNames = append(spanNames, span)
		}

		sort.Slice(spanNames, func(i, j int) bool {
			timeI := t.spanTS[group][spanNames[i]]
			timeJ := t.spanTS[group][spanNames[j]]
			return timeI.After(timeJ) // most recent first
		})

		groupMap := make(map[string][]string)
		for j, span := range spanNames {
			if j > 0 {
				jsonBuf.WriteString(`,`)
			}
			v, _ := json.Marshal(span)
			jsonBuf.WriteString(fmt.Sprintf(`%s:`, v))

			originalEntries := spans[span]
			sortedEntries := make([]logEntry, len(originalEntries))
			copy(sortedEntries, originalEntries)
			sort.Slice(sortedEntries, func(i, j int) bool {
				return sortedEntries[i].time.After(sortedEntries[j].time) // Most recent first
			})

			formattedEntries := make([]string, 0, len(sortedEntries))
			for _, entry := range sortedEntries {
				formattedEntries = append(formattedEntries, entry.FormattedMessage(timezone, withExactTime))
			}
			groupMap[span] = formattedEntries

			vs, _ := json.Marshal(formattedEntries)
			jsonBuf.Write(vs)
		}

		jsonBuf.WriteString(`}`)

		m[group] = groupMap
	}

	jsonBuf.WriteString(`}`)

	return m, jsonBuf.Bytes()
}

func (t *tracer) Enable() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled = true
}

func (t *tracer) Disable() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled = false
}

func (t *tracer) IsEnabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.enabled
}

type logger struct {
	tracer *tracer
	group  string
	span   string
}

var _ Logger = &logger{}

func (l *logger) Span(span string) Logger {
	return &logger{
		tracer: l.tracer,
		group:  l.group,
		span:   span,
	}
}

func (l *logger) With(group, span string) Logger {
	return &logger{
		tracer: l.tracer,
		group:  group,
		span:   span,
	}
}

func (l *logger) GetGroup() string {
	return l.group
}

func (l *logger) GetSpan() string {
	return l.span
}

func (l *logger) Info(message string, v ...any) {
	l.log("INFO", l.group, l.span, message, v...)
}

func (l *logger) Warn(message string, v ...any) {
	l.log("WARN", l.group, l.span, message, v...)
}

func (l *logger) Error(message string, v ...any) {
	l.log("ERROR", l.group, l.span, message, v...)
}

func (l *logger) log(level, group, span, message string, v ...any) {
	if !l.tracer.IsEnabled() {
		return
	}

	l.tracer.mu.Lock()
	defer l.tracer.mu.Unlock()

	timeNow := time.Now().UTC()

	// Ensure group exists and handle group limit
	if _, ok := l.tracer.logs[group]; !ok {
		if len(l.tracer.groupTS) >= l.tracer.numGroups && l.tracer.numGroups > 0 {
			// Find and remove the oldest group
			var oldestGroup string
			var oldestTime time.Time
			first := true
			for grp, ts := range l.tracer.groupTS {
				if first || ts.Before(oldestTime) {
					oldestGroup = grp
					oldestTime = ts
					first = false
				}
			}
			if oldestGroup != "" { // Ensure we found one
				delete(l.tracer.logs, oldestGroup)
				delete(l.tracer.groupTS, oldestGroup)
				delete(l.tracer.spanTS, oldestGroup)
			}
		}
		// Create the new group structures
		l.tracer.logs[group] = make(map[string][]logEntry)
		l.tracer.spanTS[group] = make(map[string]time.Time)
	}
	// Update group timestamp regardless of whether it was new or existing
	l.tracer.groupTS[group] = timeNow

	// Ensure span exists and handle span limit
	_, spanExists := l.tracer.logs[group][span]
	if !spanExists {
		if len(l.tracer.spanTS[group]) >= l.tracer.numSpans && l.tracer.numSpans > 0 {
			// Find and remove the oldest span in this group
			var oldestSpan string
			var oldestTime time.Time
			first := true
			for sp, ts := range l.tracer.spanTS[group] {
				if first || ts.Before(oldestTime) {
					oldestSpan = sp
					oldestTime = ts
					first = false
				}
			}
			if oldestSpan != "" { // Ensure we found one
				delete(l.tracer.logs[group], oldestSpan)
				delete(l.tracer.spanTS[group], oldestSpan)
			}
		}
		// Create the new span slice (it will be populated later)
		// Ensure the map entry exists even if the slice is initially empty
		l.tracer.logs[group][span] = make([]logEntry, 0, l.tracer.numMessages)
	}
	// Update span timestamp regardless of whether it was new or existing
	l.tracer.spanTS[group][span] = timeNow

	// Log entry handling
	s := l.tracer.logs[group][span] // Get the (potentially new) span slice

	// Format message and apply length limit
	msg := fmt.Sprintf(message, v...)
	if len(msg) == 0 {
		return // Don't log empty messages
	}
	const maxMsgLen = 1000
	if len(msg) > maxMsgLen {
		msg = msg[:maxMsgLen] // truncate
	}

	// Check for duplicate message to increment count instead of adding new entry
	found := false
	for i := range s {
		// Check level as well to differentiate INFO/WARN/ERROR of same message
		if s[i].message == msg && s[i].level == level {
			s[i].count++
			s[i].time = timeNow
			l.tracer.logs[group][span] = s
			found = true
			break
		}
	}

	// If it wasn't a duplicate, add a new entry
	if !found {
		newEntry := logEntry{
			group:   l.group,
			span:    l.span,
			message: msg,
			level:   level,
			time:    timeNow,
			count:   1,
		}
		// Handle message limit using FIFO eviction
		if len(s) < l.tracer.numMessages {
			s = append(s, newEntry)
		} else if l.tracer.numMessages > 0 {
			s = append(s[1:], newEntry)
		} else {
			// If numMessages is 0, effectively disable message logging for this span
			s = []logEntry{}
		}
		l.tracer.logs[group][span] = s
	}
}

type logEntry struct {
	group   string
	span    string
	message string
	level   string
	time    time.Time
	count   uint32
}

var _ LogEntry = logEntry{}

func (l logEntry) Group() string {
	return l.group
}

func (l logEntry) Span() string {
	return l.span
}

func (l logEntry) Message() string {
	return l.message
}

func (l logEntry) Level() string {
	return l.level
}

func (l logEntry) Time() time.Time {
	return l.time
}

func (l logEntry) TimeAgo(timezone ...string) string {
	var err error
	loc := time.UTC
	if len(timezone) > 0 {
		loc, err = time.LoadLocation(timezone[0])
		if err != nil {
			loc = time.UTC
		}
	}

	duration := time.Since(l.time.In(loc))

	if duration < time.Minute {
		return fmt.Sprintf("%ds ago", int(duration.Seconds()))
	} else if duration < time.Hour {
		seconds := int(duration.Seconds()) % 60
		return fmt.Sprintf("%dm %ds ago", int(duration.Minutes()), seconds)
	} else if duration < 24*time.Hour {
		hours := int(duration.Hours())
		minutes := int(duration.Minutes()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%dh ago", hours)
		}
		return fmt.Sprintf("%dh %dm ago", hours, minutes)
	}

	return l.time.In(loc).Format(time.RFC822)
}

func (l logEntry) Count() uint32 {
	return l.count
}

func (l logEntry) FormattedMessage(timezone string, withExactTime ...bool) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	var out string
	if len(withExactTime) > 0 && withExactTime[0] {
		out = fmt.Sprintf("%s - [%s] %s", l.time.In(loc).Format(time.RFC822), l.level, l.message)
	} else {
		out = fmt.Sprintf("%s - [%s] %s", l.TimeAgo(timezone), l.level, l.message)
	}
	if l.count > 1 {
		return fmt.Sprintf("%s [x%d]", out, l.count)
	} else {
		return out
	}
}
