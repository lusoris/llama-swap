package main

import (
	"fmt"
	"log"
	"strings"
)

// logf logs like log.Printf with CR and LF escaped, so a flag value or an API error cannot start a forged log line.
func logf(format string, args ...any) {
	log.Print(strings.ReplaceAll(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", `\n`), "\r", `\r`))
}
