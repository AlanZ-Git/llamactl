package instance

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	timber "github.com/DeRuina/timberjack"
)

// LogRotationConfig contains log rotation settings for instances
type LogRotationConfig struct {
	Enabled  bool
	MaxSize  int
	Compress bool
}

type logger struct {
	name        string
	logDir      string
	logFile     *timber.Logger
	logFilePath string
	mu          sync.RWMutex
	cfg         *LogRotationConfig
}

func newLogger(name, logDir string, cfg *LogRotationConfig) *logger {
	return &logger{
		name:   name,
		logDir: logDir,
		cfg:    cfg,
	}
}

func (l *logger) create() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.logDir == "" {
		return fmt.Errorf("logDir empty for instance %s", l.name)
	}

	if err := os.MkdirAll(l.logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := fmt.Sprintf("%s/%s.log", l.logDir, l.name)
	l.logFilePath = logPath

	// Build the timber logger
	t := &timber.Logger{
		Filename:   logPath,
		MaxSize:    l.cfg.MaxSize,
		MaxBackups: 0, // No limit on backups
		// Compression: "gzip" if Compress is true, else "none"
		Compression: func() string {
			if l.cfg.Compress {
				return "gzip"
			}
			return "none"
		}(),
		FileMode:  0644,
		LocalTime: true,
	}

	// If rotation is disabled, set MaxSize to 0 so no rotation occurs
	if !l.cfg.Enabled {
		t.MaxSize = 0
	}

	// A replacement must not leave the previous file open. Output readers carry
	// their original file identity and cannot append after this close.
	l.closeLocked(l.logFile)
	l.logFile = t

	// Write a startup marker
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(t, "\n=== Instance %s started at %s ===\n", l.name, ts)

	return nil
}

// startOutput captures the log generation before scheduling either reader. Its
// cleanup closes only those pipes, joins both readers, then closes that log.
func (l *logger) startOutput(readers ...io.ReadCloser) func() {
	l.mu.RLock()
	lg := l.logFile
	l.mu.RUnlock()
	var wg sync.WaitGroup
	for _, rc := range readers {
		wg.Add(1)
		go func() { defer wg.Done(); l.readOutputGeneration(rc, lg) }()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, rc := range readers {
				_ = rc.Close()
			}
			wg.Wait()
			l.mu.Lock()
			defer l.mu.Unlock()
			l.closeLocked(lg)
		})
	}
}

func (l *logger) readOutput(rc io.ReadCloser) {
	l.mu.RLock()
	lg := l.logFile
	l.mu.RUnlock()
	l.readOutputGeneration(rc, lg)
}

func (l *logger) readOutputGeneration(rc io.ReadCloser, lg *timber.Logger) {
	defer rc.Close()
	scanner := bufio.NewScanner(rc)
	for scanner.Scan() {
		l.mu.RLock()
		if lg != nil && l.logFile == lg {
			fmt.Fprintln(lg, scanner.Text())
		}
		l.mu.RUnlock()
	}
	if err := scanner.Err(); err != nil {
		l.mu.RLock()
		if lg != nil && l.logFile == lg {
			fmt.Fprintf(lg, "\n=== Error reading output for instance %s: %v ===\n", l.name, err)
		}
		l.mu.RUnlock()
	}
}

func (l *logger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeLocked(l.logFile)
}

// Caller holds mu exclusively. Never write a stop marker through a stale timber
// logger: Write could reopen its closed file. A stale close cannot clear the new log.
func (l *logger) closeLocked(lg *timber.Logger) {
	if lg == nil {
		return
	}
	if l.logFile == lg {
		ts := time.Now().Format("2006-01-02 15:04:05")
		fmt.Fprintf(lg, "=== Instance %s stopped at %s ===\n\n", l.name, ts)
		l.logFile = nil
	}
	_ = lg.Close()
}

// getLogs retrieves the last n lines of logs from the instance
func (l *logger) getLogs(num_lines int) (string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.logFilePath == "" {
		return "", fmt.Errorf("log file not created for instance %s", l.name)
	}

	file, err := os.Open(l.logFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to open log file: %w", err)
	}
	defer file.Close()

	if num_lines <= 0 {
		content, err := io.ReadAll(file)
		if err != nil {
			return "", fmt.Errorf("failed to read log file: %w", err)
		}
		return string(content), nil
	}

	var lines []string
	scanner := bufio.NewScanner(file)

	// Read all lines into a slice
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}

	// Return the last N lines
	start := max(len(lines)-num_lines, 0)

	return strings.Join(lines[start:], "\n"), nil
}
