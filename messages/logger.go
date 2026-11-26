package messages

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

var logLevelToInt = map[string]int{
	"":      -1,
	"DEBUG": 0,
	"INFO":  1,
	"WARN":  2,
	"ERROR": 3,
}

type fileLogger struct {
	file *os.File
	mu   *sync.Mutex
	mod  string
	min  int
}

// NewFileLogger creates a waLog.Logger that writes timestamped log lines to a file.
func NewFileLogger(filePath, module, minLevel string) (waLog.Logger, io.Closer, error) {
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	min, ok := logLevelToInt[strings.ToUpper(minLevel)]
	if !ok {
		min = 0
	}
	return &fileLogger{
		file: f,
		mu:   &sync.Mutex{},
		mod:  module,
		min:  min,
	}, f, nil
}

func (l *fileLogger) outputf(level, msg string, args ...any) {
	if logLevelToInt[level] < l.min {
		return
	}
	line := fmt.Sprintf("%s [%s %s] %s\n", time.Now().Format("2006-01-02 15:04:05.000"), l.mod, level, fmt.Sprintf(msg, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.file.WriteString(line)
}

func (l *fileLogger) Errorf(msg string, args ...any) { l.outputf("ERROR", msg, args...) }
func (l *fileLogger) Warnf(msg string, args ...any)  { l.outputf("WARN", msg, args...) }
func (l *fileLogger) Infof(msg string, args ...any)  { l.outputf("INFO", msg, args...) }
func (l *fileLogger) Debugf(msg string, args ...any) { l.outputf("DEBUG", msg, args...) }
func (l *fileLogger) Sub(mod string) waLog.Logger {
	return &fileLogger{
		file: l.file,
		mu:   l.mu,
		mod:  l.mod + "/" + mod,
		min:  l.min,
	}
}
