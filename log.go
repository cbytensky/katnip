package main

import (
	"flag"
	"fmt"
	"os"
)

// Log levels
const (
	LogOff = iota
	LogErr
	LogWrn
	LogInf
	LogDbg
	LogTrc
)

var LogLevel *int
var LogUseColor *bool

func AddFlagLog() {
	LogLevel = flag.Int("loglevel", LogInf, "Log level (off = 0, error = 1, warning = 2, info = 3, debug = 4, trace = 5)")
	LogUseColor = flag.Bool("color", false, "Use color for logging")
}

func Log(level byte, format string, args ...interface{}) {
	var colorPrefix, colorSuffix string
	if level <= byte(*LogLevel) {
		if *LogUseColor {
			colorPrefix = "\033[" + [...]string{"", "91", "93", "0", "97", "94"}[level] + "m"
			colorSuffix = "\033[0m"
		}
		_, _ = fmt.Fprintf(os.Stderr, colorPrefix+[...]string{"", "ERR", "WRN", "INF", "DBG", "TRC"}[level]+" "+format+colorSuffix+"\n", args...)
	}
}
