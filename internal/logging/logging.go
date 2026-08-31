// Package logging provides Hole's logging: a console handler that reproduces the
// `[LEVEL] message` style users know, plus an optional per-run JSON file handler that
// always records debug detail (engine command lines, durations).
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	colorInfo  = "\033[0;32m"
	colorWarn  = "\033[0;33m"
	colorError = "\033[0;31m"
	colorDebug = "\033[0;36m"
	colorReset = "\033[0m"
)

// consoleHandler renders records as `[LEVEL] message` on stderr, colored when stderr is a
// terminal. Attributes are intentionally dropped — the file handler keeps the full record.
type consoleHandler struct {
	mu     *sync.Mutex
	out    *os.File
	colors bool
	level  slog.Level
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, rec slog.Record) error {
	label, color := "INFO", colorInfo
	switch {
	case rec.Level >= slog.LevelError:
		label, color = "ERROR", colorError
	case rec.Level >= slog.LevelWarn:
		label, color = "WARN", colorWarn
	case rec.Level < slog.LevelInfo:
		label, color = "DEBUG", colorDebug
	}
	if !h.colors {
		color = ""
	}
	reset := colorReset
	if !h.colors {
		reset = ""
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if rec.Message == "" {
		_, err := fmt.Fprintln(h.out)
		return err
	}
	_, err := fmt.Fprintf(h.out, "%s[%s] %s%s\n", color, label, rec.Message, reset)
	return err
}

func (h *consoleHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *consoleHandler) WithGroup(_ string) slog.Handler      { return h }

// fanoutHandler dispatches every record to all wrapped handlers.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, inner := range h.handlers {
		if inner.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, inner := range h.handlers {
		if !inner.Enabled(ctx, rec.Level) {
			continue
		}
		if err := inner.Handle(ctx, rec.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, inner := range h.handlers {
		next = append(next, inner.WithAttrs(attrs))
	}
	return &fanoutHandler{handlers: next}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, inner := range h.handlers {
		next = append(next, inner.WithGroup(name))
	}
	return &fanoutHandler{handlers: next}
}

// Options configures Setup.
type Options struct {
	// Debug lowers the console level to debug.
	Debug bool
	// LogFile, when non-empty, receives a JSON record stream at debug level.
	LogFile string
	// NoColor forces plain output regardless of terminal detection.
	NoColor bool
	// Quiet drops the console handler entirely. The watchdog uses it: its stderr already
	// points at the run log, so console output would duplicate the file records.
	Quiet bool
}

// Setup installs the process-wide logger and returns a closer for the log file (if any).
func Setup(opts Options) (func(), error) {
	consoleLevel := slog.LevelInfo
	if opts.Debug {
		consoleLevel = slog.LevelDebug
	}

	colors := !opts.NoColor
	if colors {
		if info, err := os.Stderr.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
			colors = false
		}
	}

	var handlers []slog.Handler
	relayHandler = nil
	if !opts.Quiet {
		console := &consoleHandler{
			mu:     &sync.Mutex{},
			out:    os.Stderr,
			colors: colors,
			level:  consoleLevel,
		}
		handlers = append(handlers, console)
		relayHandler = console
	}

	closer := func() {}
	if opts.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(opts.LogFile), 0o755); err != nil {
			return closer, fmt.Errorf("create log directory: %w", err)
		}
		file, err := os.OpenFile(opts.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return closer, fmt.Errorf("open log file: %w", err)
		}
		handlers = append(handlers, slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
		closer = func() { _ = file.Close() }
	}

	slog.SetDefault(slog.New(&fanoutHandler{handlers: handlers}))
	return closer, nil
}

// relayHandler is the console handler installed by Setup. Relay writes through it directly
// so mirrored records reach the terminal without being appended to the log file a second
// time (they are already in there — that is where they came from).
var relayHandler slog.Handler

// Relay prints one record that originated in another process, keeping its level.
func Relay(level, message string) {
	if relayHandler == nil || message == "" {
		return
	}
	slogLevel := slog.LevelInfo
	switch level {
	case "ERROR":
		slogLevel = slog.LevelError
	case "WARN":
		slogLevel = slog.LevelWarn
	case "DEBUG":
		slogLevel = slog.LevelDebug
	}
	if !relayHandler.Enabled(context.Background(), slogLevel) {
		return
	}
	_ = relayHandler.Handle(context.Background(), slog.NewRecord(time.Now(), slogLevel, message, 0))
}

// SetComponent tags every subsequent record with a component name. The console handler
// ignores attributes, so this only shows up in the JSON log — which is what lets the CLI
// relay exactly the watchdog's records and none of its own.
func SetComponent(name string) {
	slog.SetDefault(slog.New(slog.Default().Handler().WithAttrs([]slog.Attr{slog.String("component", name)})))
}

// Info logs at info level; arguments follow fmt.Sprintf semantics.
func Info(format string, args ...any) { logf(slog.LevelInfo, format, args...) }

// Warn logs a user-config problem Hole recovered from by skipping something.
func Warn(format string, args ...any) { logf(slog.LevelWarn, format, args...) }

// Error logs a problem that makes the sandbox wrong or unsafe.
func Error(format string, args ...any) { logf(slog.LevelError, format, args...) }

// Debug logs detail that always reaches the run log file but the console only with -d.
func Debug(format string, args ...any) { logf(slog.LevelDebug, format, args...) }

// Line emits a blank separator line, matching the bash CLI's log_line.
func Line() { logf(slog.LevelInfo, "") }

func logf(level slog.Level, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	logger := slog.Default()
	if !logger.Enabled(context.Background(), level) {
		return
	}
	rec := slog.NewRecord(time.Now(), level, msg, 0)
	_ = logger.Handler().Handle(context.Background(), rec)
}

// LogFileGC removes run log files older than the retention period. Best-effort.
func LogFileGC(dir string, retention time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-retention)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}
