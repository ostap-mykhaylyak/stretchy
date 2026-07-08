// Package logx is a minimal leveled logger writing to
// <dir>/stretchy.log, falling back to stderr when no directory is set.
package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func parseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return Debug
	case "warn", "warning":
		return Warn
	case "error":
		return Error
	default:
		return Info
	}
}

type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	file  *os.File
	level Level
}

func New(dir, level string) (*Logger, error) {
	l := &Logger{out: os.Stderr, level: parseLevel(level)}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, "stretchy.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		l.file = f
		l.out = f
	}
	return l, nil
}

func (l *Logger) log(lv Level, tag, format string, args ...interface{}) {
	if lv < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s [%s] %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), tag, fmt.Sprintf(format, args...))
}

func (l *Logger) Debug(format string, args ...interface{}) { l.log(Debug, "DEBUG", format, args...) }
func (l *Logger) Info(format string, args ...interface{})  { l.log(Info, "INFO", format, args...) }
func (l *Logger) Warn(format string, args ...interface{})  { l.log(Warn, "WARN", format, args...) }
func (l *Logger) Error(format string, args ...interface{}) { l.log(Error, "ERROR", format, args...) }

func (l *Logger) Close() {
	if l.file != nil {
		l.file.Close()
	}
}
