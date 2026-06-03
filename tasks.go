package main

import (
	"bufio"
	"fmt"
	"strings"
	"time"
)

func createTask(args []string) {
	text := strings.Join(args, " ")
	ts := time.Now().Format(timestampLayout)
	line := fmt.Sprintf("%s[%s]%s [TASK] %s\n", ColorCyan, ts, ColorReset, text)
	appendLine(line)
	fmt.Println(ColorGreen + "Task created and pinned to board!" + ColorReset)
	syncTaskToGoogle(text)
}

func viewTasks() {
	f, ok := openNotesFile("No tasks yet. Create one with: jot task <text>")
	if !ok {
		return
	}
	defer f.Close()

	fmt.Println(ColorGreen + "--- Tasks ---" + ColorReset)

	sc := bufio.NewScanner(f)
	pending, done := 0, 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "[TASK:DONE]"):
			fmt.Println(ColorDim + "  [x] " + line + ColorReset)
			done++
		case strings.Contains(line, "[TASK]"):
			fmt.Println(ColorGreen + "  [ ] " + line + ColorReset)
			pending++
		}
	}
	check(sc.Err(), "Error reading file")

	if pending == 0 && done == 0 {
		fmt.Println("  No tasks yet. Create one with: jot task <text>")
	} else {
		fmt.Printf("\n  %d pending, %d done\n", pending, done)
	}
}

func markTaskDone(term string) {
	marked, ok := processLines(func(line string) (string, bool) {
		if strings.Contains(strings.ToLower(line), strings.ToLower(term)) &&
			strings.Contains(line, "[TASK]") &&
			!strings.Contains(line, "[TASK:DONE]") {
			return strings.Replace(line, "[TASK]", "[TASK:DONE]", 1), true
		}
		return line, false
	})
	if !ok {
		return
	}
	if marked > 0 {
		fmt.Printf(ColorGreen+"Marked %d task(s) as done!\n"+ColorReset, marked)
	} else {
		fmt.Println("No matching pending tasks found.")
	}
}

func clearDoneTasks() {
	cleared, ok := processLines(func(line string) (string, bool) {
		if strings.Contains(line, "[TASK:DONE]") {
			return "", true
		}
		return line, false
	})
	if !ok {
		return
	}
	fmt.Printf("Cleared %d completed task(s).\n", cleared)
}
