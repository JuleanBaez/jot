package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

func getPath() string {
	if path := os.Getenv("JOT_PATH"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: home directory:", err)
		os.Exit(1)
	}
	return home + defaultFileName
}

func userInput(parts []string) {
	note := strings.Join(parts, " ")
	ts := time.Now().Format(timestampLayout)
	appendLine(fmt.Sprintf("%s[%s]%s %s\n", ColorCyan, ts, ColorReset, note))
	fmt.Println("Note jotted!")
}

func appendLine(line string) {
	f, err := os.OpenFile(getPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	check(err, "Could not open file")
	defer f.Close()
	_, err = f.WriteString(line)
	check(err, "Error writing note")
}

func openNotesFile(msg string) (*os.File, bool) {
	f, err := os.Open(getPath())
	if os.IsNotExist(err) {
		fmt.Println(msg)
		return nil, false
	}
	check(err, "Failed to open file")
	return f, true
}

// processLines: fn(line) returns (replacement, acted); acted=true+empty drops the line.
func processLines(fn func(string) (string, bool)) (int, bool) {
	filePath := getPath()
	temp := filePath + ".tmp"

	orig, err := os.Open(filePath)
	if os.IsNotExist(err) {
		fmt.Println(noNotes)
		return 0, false
	}
	check(err, "Could not open file")

	tmp, err := os.Create(temp)
	check(err, "Could not create temp file")

	sc := bufio.NewScanner(orig)
	count := 0
	for sc.Scan() {
		line := sc.Text()
		out, acted := fn(line)
		if acted {
			count++
			if out != "" {
				tmp.WriteString(out + "\n")
			}
		} else {
			tmp.WriteString(line + "\n")
		}
	}
	tmp.Close()
	orig.Close()
	check(sc.Err(), "Error reading file")
	check(os.Rename(temp, filePath), "Could not save changes")
	return count, true
}
