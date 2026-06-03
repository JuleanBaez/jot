package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"jot/dash"
)

const (
	timestampLayout = "2006-01-02 15:04:05"
	defaultFileName = "/Documents/jot.txt"
	ColorCyan       = "\033[36m"
	ColorReset      = "\033[0m"
	ColorYellow     = "\033[33m"
	ColorGreen      = "\033[32m"
	ColorDim        = "\033[2m"
	noNotes         = "No notes found. Try: jot <text>"
)

func check(err error, message string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", message, err)
		os.Exit(1)
	}
}

func parseDashFlags(args []string) dash.Options {
	var opts dash.Options
	for _, a := range args {
		switch a {
		case "--no-open", "--headless":
			opts.NoOpen = true
		case "--help", "-h":
			fmt.Print("Usage: jot dash [--no-open]\n\n" +
				"  Starts the local dashboard (default http://127.0.0.1:8787)\n" +
				"  and opens it in your browser. Press Ctrl-C to stop.\n\n" +
				"  --no-open    Do not launch the browser (useful over SSH).\n")
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "jot dash: unknown flag %q (try --help)\n", a)
			os.Exit(2)
		}
	}
	return opts
}

func main() {
	args := os.Args[1:]

	if len(args) == 0 {
		printHelp()
		os.Exit(0)
	}

	switch args[0] {
	case "view":
		viewNote()
	case "search":
		if len(args) < 2 {
			fmt.Println("Usage: jot search <term>")
			os.Exit(1)
		}
		searchNote(strings.Join(args[1:], " "))
	case "delete":
		if len(args) < 2 {
			fmt.Println("Usage: jot delete <term>")
			os.Exit(1)
		}
		deleteNote(strings.Join(args[1:], " "))
	case "tail":
		n := 5
		if len(args) > 1 {
			parsed, err := strconv.Atoi(args[1])
			if err == nil && parsed > 0 {
				n = parsed
			} else {
				fmt.Println("Invalid number: using default of 5.")
			}
		}
		tailNote(n)
	case "export":
		exportJSON()
	case "help":
		printHelp()
	case "pin":
		if len(args) < 2 {
			fmt.Println("Usage: jot pin <text>")
			os.Exit(1)
		}
		userInput(append([]string{"[PIN]"}, args[1:]...))
	case "board":
		viewBoard()
	case "task":
		if len(args) < 2 {
			fmt.Println("Usage: jot task <text>")
			os.Exit(1)
		}
		createTask(args[1:])
	case "tasks":
		viewTasks()
	case "done":
		if len(args) < 2 {
			fmt.Println("Usage: jot done <term>")
			os.Exit(1)
		}
		markTaskDone(strings.Join(args[1:], " "))
	case "clear":
		clearDoneTasks()
	case "auth":
		setupGoogleAuth()
	case "ad":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad <command> [args]")
			fmt.Println("       jot ad help  — list AD commands")
			os.Exit(1)
		}
		handleAD(args[1:])
	case "onboard":
		handleOnboard(args[1:])
	case "dash":
		opts := parseDashFlags(args[1:])
		if err := dash.Run(opts); err != nil {
			fmt.Fprintf(os.Stderr, "dash: %v\n", err)
			os.Exit(1)
		}
	case "iiq":
		if len(args) < 2 {
			fmt.Println("Usage: jot iiq debug [flags]")
			os.Exit(1)
		}
		switch args[1] {
		case "debug":
			handleIIQDebug(args[2:])
		default:
			fmt.Fprintf(os.Stderr, "jot iiq: unknown subcommand %q\n", args[1])
			os.Exit(2)
		}
	default:
		userInput(args)
	}
}
