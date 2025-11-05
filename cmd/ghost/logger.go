package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var logMu sync.Mutex

func logInfo(format string, args ...any) {
	logWithWriter(os.Stdout, format, args...)
}

func logError(format string, args ...any) {
	logWithWriter(os.Stderr, format, args...)
}

func logWithWriter(writer *os.File, format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()

	timestamp := time.Now().Format("15:04:05.000")
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(writer, "[ghost %s] %s\n", timestamp, message)
}
