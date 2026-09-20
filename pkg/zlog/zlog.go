// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package zlog is the logging facade used across the daemon and the plugin.
//
// The plugin writes to a file rather than to standard error: its output is
// consumed by the container runtime, and anything it prints there risks being
// parsed as part of the CNI result.
package zlog

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/natefinch/lumberjack"
	"github.com/sirupsen/logrus"
)

const (
	// EnvLogLevel and EnvLogLocation are read once at init. They are exported so
	// a binary that also takes a log-level flag can read and name the same
	// variable instead of hardcoding the string (see cmd/cni-ipam).
	EnvLogLevel    = "ZENLAYER_CNI_LOGLEVEL"
	EnvLogLocation = "ZENLAYER_CNI_LOCATION"
)

var levelMap = map[string]logrus.Level{
	"trace": logrus.TraceLevel,
	"debug": logrus.DebugLevel,
	"info":  logrus.InfoLevel,
	"warn":  logrus.WarnLevel,
	"error": logrus.ErrorLevel,
	"fatal": logrus.FatalLevel,
	"panic": logrus.PanicLevel,
}

var (
	logLevel    string
	logFilePath string

	// log is created lazily by GetLogger and never replaced. There is no lock:
	// callers take it in package-level var initialisers, which run before main
	// and one at a time.
	log Logger
)

func init() {
	logLevel = os.Getenv(EnvLogLevel)
	logFilePath = os.Getenv(EnvLogLocation)
}

// isValidFile reports whether path can be used as a log file.
//
// A path that does not exist yet counts as valid: lumberjack creates the file,
// and its parent directory, on the first write. Treating "not there" as invalid
// meant the very first run of a fresh node fell back to stderr -- and for the
// CNI plugin, whose stderr the runtime discards, that threw every line away.
// An existing directory is unusable, and so is a path Stat cannot resolve.
func isValidFile(path string) bool {
	s, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	return !s.IsDir()
}

func getLogLocation(logLocation string) io.Writer {
	if strings.ToLower(logLocation) == "stdout" {
		return os.Stdout
	}

	if !isValidFile(logLocation) {
		return os.Stderr
	}
	return &lumberjack.Logger{
		Filename:   logLocation,
		MaxSize:    256,
		MaxAge:     7,
		MaxBackups: 2,
		Compress:   true,
	}
}

// Fields carries the key/value pairs of a structured log record.
type Fields map[string]interface{}

type Logger interface {
	Debugf(format string, args ...interface{})

	Debug(args ...interface{})

	DebugWithFields(fields Fields, args ...interface{})

	Infof(format string, args ...interface{})

	Info(args ...interface{})

	InfoWithFields(fields Fields, args ...interface{})

	Warnf(format string, args ...interface{})

	Warn(args ...interface{})

	WarnWithFields(fields Fields, args ...interface{})

	Errorf(format string, args ...interface{})

	Error(args ...interface{})

	ErrorWithFields(fields Fields, args ...interface{})

	Fatalf(format string, args ...interface{})

	Panicf(format string, args ...interface{})

	WithFields(fields Fields) Logger

	WithField(key string, value interface{}) Logger

	WithError(err error) Logger

	SetLogLevel(level string) error
}

type formatLogger struct {
	entry *logrus.Entry
}

func (logf *formatLogger) Debugf(format string, args ...interface{}) {
	logf.entry.Debugf(format, args...)
}

func (logf *formatLogger) Debug(args ...interface{}) {
	logf.entry.Debug(args...)
}

func (logf *formatLogger) DebugWithFields(fields Fields, args ...interface{}) {
	logf.entry.WithFields(logrus.Fields(fields)).Debug(args...)
}

func (logf *formatLogger) Infof(format string, args ...interface{}) {
	logf.entry.Infof(format, args...)
}

func (logf *formatLogger) Info(args ...interface{}) {
	logf.entry.Info(args...)
}

func (logf *formatLogger) InfoWithFields(fields Fields, args ...interface{}) {
	logf.entry.WithFields(logrus.Fields(fields)).Info(args...)
}

func (logf *formatLogger) Warnf(format string, args ...interface{}) {
	logf.entry.Warnf(format, args...)
}

func (logf *formatLogger) Warn(args ...interface{}) {
	logf.entry.Warn(args...)
}

func (logf *formatLogger) WarnWithFields(fields Fields, args ...interface{}) {
	logf.entry.WithFields(logrus.Fields(fields)).Warn(args...)
}

func (logf *formatLogger) Errorf(format string, args ...interface{}) {
	logf.entry.Errorf(format, args...)
}

func (logf *formatLogger) Error(args ...interface{}) {
	logf.entry.Error(args...)
}

func (logf *formatLogger) ErrorWithFields(fields Fields, args ...interface{}) {
	logf.entry.WithFields(logrus.Fields(fields)).Error(args...)
}

func (logf *formatLogger) Fatalf(format string, args ...interface{}) {
	logf.entry.Fatalf(format, args...)
}

func (logf *formatLogger) Panicf(format string, args ...interface{}) {
	logf.entry.Panicf(format, args...)
}

func (logf *formatLogger) WithFields(fields Fields) Logger {
	entry := logf.entry.WithFields(logrus.Fields(fields))
	return &formatLogger{entry}
}

func (logf *formatLogger) WithField(k string, v interface{}) Logger {
	entry := logf.entry.WithField(k, v)
	return &formatLogger{entry}
}

func (logf *formatLogger) WithError(err error) Logger {
	entry := logf.entry.WithError(err)
	return &formatLogger{entry}
}

func (logf *formatLogger) SetLogLevel(level string) error {
	l, err := logrus.ParseLevel(strings.ToLower(level))
	if err != nil {
		return err
	}
	logf.entry.Logger.SetLevel(l)
	return nil
}

// parseLevel maps the configured level name onto a logrus level, falling back
// to info for anything unrecognized.
func parseLevel(level string) logrus.Level {
	if l, ok := levelMap[strings.ToLower(strings.TrimSpace(level))]; ok {
		return l
	}
	return logrus.InfoLevel
}

// callerFieldName is the key the call site is logged under.
const callerFieldName = "source"

// callerHook stamps every record with the file:line that emitted it.
//
// logrus only fires hooks for records that pass the level filter, so records
// below the configured level cost nothing.
type callerHook struct {
	Field     string
	Skip      int
	levels    []logrus.Level
	Formatter func(file, function string, line int) string
}

func NewCallerHook(levels ...logrus.Level) *callerHook {
	hook := &callerHook{
		Field: callerFieldName,
		// Where the stack walk starts. findCaller skips wrapper frames by
		// function name, so this only saves a few iterations.
		Skip:   3,
		levels: levels,
		Formatter: func(file, function string, line int) string {
			return fmt.Sprintf("%s:%d", file, line)
		},
	}
	if len(hook.levels) == 0 {
		hook.levels = logrus.AllLevels
	}
	return hook
}

func (hook *callerHook) Levels() []logrus.Level {
	return hook.levels
}

func (hook *callerHook) Fire(entry *logrus.Entry) error {
	entry.Data[hook.Field] = hook.Formatter(findCaller(hook.Skip))
	return nil
}

// pkgPrefix is this package's symbol prefix, e.g.
// "github.com/zenlayer/.../pkg/zlog.". Frames are matched on the function name
// rather than the file path: a path-prefix check would also swallow any caller
// that happens to sit in a directory called zlog, and it silently breaks if the
// package is ever split across files.
var pkgPrefix = func() string {
	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	name := runtime.FuncForPC(pc).Name()
	// name looks like "<pkg-path>.glob..func1"; cut at the last path segment.
	if i := strings.LastIndex(name, "/"); i >= 0 {
		if j := strings.Index(name[i:], "."); j >= 0 {
			return name[:i+j+1]
		}
	}
	return ""
}()

func isWrapperFrame(fnName string) bool {
	if pkgPrefix != "" && strings.HasPrefix(fnName, pkgPrefix) {
		return true
	}
	return strings.Contains(fnName, "github.com/sirupsen/logrus") ||
		strings.HasPrefix(fnName, "runtime.")
}

// findCaller walks up the stack until it leaves logrus and this package, and
// returns the first frame that belongs to actual calling code.
//
// skip only decides where the search starts. Starting too low costs an extra
// iteration, because wrapper frames are skipped by name anyway.
func findCaller(skip int) (string, string, int) {
	for i := 0; i < 20; i++ {
		pc, f, l, ok := runtime.Caller(skip + i)
		if !ok {
			break
		}
		fnName := runtime.FuncForPC(pc).Name()
		if isWrapperFrame(fnName) {
			continue
		}
		return trimPath(f), fnName, l
	}
	return "", "", 0
}

// trimPath shortens an absolute path to its last two segments, e.g.
// "daemon/eni_multi_ip.go".
func trimPath(f string) string {
	n := 0
	for i := len(f) - 1; i >= 0; i-- {
		if f[i] == '/' {
			n++
			if n >= 2 {
				return f[i+1:]
			}
		}
	}
	return f
}

// New returns a logger writing to logLocation, falling back to the path in
// EnvLogLocation and, when that is empty too, to logrus' own default output.
func New(logLocation string) *formatLogger {
	if logLocation == "" {
		logLocation = logFilePath
	}
	logger := logrus.New()
	if logLocation != "" {
		logger.SetOutput(getLogLocation(logLocation))
	}
	// Honor ZENLAYER_CNI_LOGLEVEL. This used to be hardcoded to Debug, which
	// left levelMap and logLevel unread and every binary logging at debug.
	logger.SetLevel(parseLevel(logLevel))
	// Attach the call site to every record. A hook is the only place this can
	// be done correctly: formatLogger wraps logrus, so logrus' own
	// SetReportCaller would report zlog.go instead of the real caller.
	logger.AddHook(NewCallerHook())
	return &formatLogger{entry: logrus.NewEntry(logger)}
}

// GetLogger returns the shared logger, creating it on first use.
func GetLogger() Logger {
	if log == nil {
		log = New(logFilePath)
	}
	return log
}
