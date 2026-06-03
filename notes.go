package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Note struct {
	Timestamp string `json:"timestamp"`
	Content   string `json:"content"`
	Type      string `json:"type"`
	Done      bool   `json:"done,omitempty"`
}

func viewNote() {
	f, ok := openNotesFile(noNotes)
	if !ok {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "[TASK:DONE]"):
			fmt.Println(ColorDim + line + ColorReset)
		case strings.Contains(line, "[TASK]"):
			fmt.Println(ColorGreen + line + ColorReset)
		case strings.Contains(line, "[PIN]"):
			fmt.Println(ColorYellow + line + ColorReset)
		default:
			fmt.Println(line)
		}
	}
	check(sc.Err(), "Error reading file")
}

func searchNote(term string) {
	f, ok := openNotesFile(noNotes)
	if !ok {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	found := false
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(strings.ToLower(line), strings.ToLower(term)) {
			fmt.Println(line)
			found = true
		}
	}
	check(sc.Err(), "Error reading file")
	if !found {
		fmt.Printf("No notes found matching '%s'.\n", term)
	}
}

func deleteNote(term string) {
	deleted, ok := processLines(func(line string) (string, bool) {
		if strings.Contains(strings.ToLower(line), strings.ToLower(term)) {
			return "", true
		}
		return line, false
	})
	if !ok {
		return
	}
	fmt.Printf("Deleted %d note(s) matching '%s'.\n", deleted, term)
}

func tailNote(n int) {
	f, ok := openNotesFile(noNotes)
	if !ok {
		return
	}
	defer f.Close()

	buf := make([]string, n)
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		buf[count%n] = sc.Text()
		count++
	}
	check(sc.Err(), "Error reading file")

	if count == 0 {
		fmt.Println(noNotes)
		return
	}
	if count < n {
		for i := 0; i < count; i++ {
			fmt.Println(buf[i])
		}
	} else {
		start := count % n
		for i := 0; i < n; i++ {
			fmt.Println(buf[(start+i)%n])
		}
	}
}

func exportJSON() {
	f, ok := openNotesFile(noNotes)
	if !ok {
		return
	}
	defer f.Close()

	var notes []Note
	sc := bufio.NewScanner(f)
	delimiter := "]" + ColorReset + " "
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, delimiter, 2)
		if len(parts) != 2 {
			continue
		}
		ts := strings.Replace(parts[0], ColorCyan+"[", "", 1)
		content := parts[1]
		noteType := "note"
		done := false
		switch {
		case strings.Contains(content, "[TASK:DONE]"):
			noteType, done = "task", true
		case strings.Contains(content, "[TASK]"):
			noteType = "task"
		case strings.Contains(content, "[PIN]"):
			noteType = "pin"
		}
		notes = append(notes, Note{Timestamp: ts, Content: content, Type: noteType, Done: done})
	}
	check(sc.Err(), "Error reading file")

	data, err := json.MarshalIndent(notes, "", "  ")
	check(err, "Failed to encode JSON")
	check(os.WriteFile("jot_export.json", data, 0644), "Failed to write JSON")
	fmt.Printf("Exported %d notes to jot_export.json\n", len(notes))
}

func viewBoard() {
	f, ok := openNotesFile("Board is empty. Use 'jot pin <text>' or 'jot task <text>'.")
	if !ok {
		return
	}
	defer f.Close()

	fmt.Println(ColorYellow + "--- Board ---" + ColorReset)

	sc := bufio.NewScanner(f)
	found := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "[TASK:DONE]"):
			// done tasks don't appear on the board
		case strings.Contains(line, "[TASK]"):
			fmt.Println(ColorGreen + "  [ ] " + line + ColorReset)
			found = true
		case strings.Contains(line, "[PIN]"):
			fmt.Println(ColorYellow + "  [*] " + line + ColorReset)
			found = true
		}
	}
	check(sc.Err(), "Error reading file")

	if !found {
		fmt.Println("  Nothing here. Use 'jot pin <text>' or 'jot task <text>'.")
	}
}

func printHelp() {
	fmt.Printf(`
  %sJOT - CLI Note & Task Manager%s
  Usage: jot <command> [arguments]  or  jot "your note"

  %sNOTES%s
  jot <text>          Save a quick note
  view                Show all notes
  search <term>       Find notes containing a term
  tail [n]            Show last n notes (default: 5)
  delete <term>       Remove notes matching a term

  %sTASKS%s
  task <text>         Create a task (auto-pinned to board, syncs to Google)
  tasks               View all tasks (pending and completed)
  done <term>         Mark a matching task as done
  clear               Remove all completed tasks

  %sPINNED%s
  pin <text>          Pin an important note to the board
  board               Show all pinned notes and pending tasks

  %sDATA & SYSTEM%s
  export              Export all notes to jot_export.json
  auth                Set up Google Tasks integration
  help                Show this menu

  %sACTIVE DIRECTORY%s
  ad new              Provision staff/student account
  ad sync             Pull onboarding tickets from Incident IQ and provision
  ad term   <user>    Full offboard: disable · move to Disabled OU · suspend Google
  ad resurrect        Full onboard: re-enable account · unsuspend Google
  ad clean            Disable stale accounts (>90 days inactive)
  ad unlock <user>    Clear account lockout
  ad reset  <user>    Force password reset at next login
  ad isolate <user>   Disable account + isolate endpoints via Cortex XDR
  ad audit            Group membership audit → Google Chat report
  ad backup           Snapshot AD environment state
  ad help             Full AD command reference

  %sONBOARDING%s
  onboard <file.yaml>         Batch new-hire orchestrator:
                                1. AD account creation (LDAP + ClassLink)
                                2. Duo 2FA enrollment link → Google Chat
                                3. IIQ onboarding ticket closure
  onboard <file.yaml> --dry-run   Preview without making any changes

  %sENVIRONMENT%s
  JOT_PATH              Custom storage path (default: ~/Documents/jot.txt)
  ~/.jot/config.json    AD, Duo, Incident IQ, Google Chat, and Cortex XDR config
  JOT_DUO_SECRET_KEY    Override duo.secret_key at runtime

`,
		ColorCyan, ColorReset,    // title
		ColorCyan, ColorReset,    // NOTES
		ColorGreen, ColorReset,   // TASKS
		ColorYellow, ColorReset,  // PINNED
		ColorCyan, ColorReset,    // DATA & SYSTEM
		ColorCyan, ColorReset,    // ACTIVE DIRECTORY
		ColorCyan, ColorReset,    // ONBOARDING
		ColorCyan, ColorReset,    // ENVIRONMENT
	)
}
