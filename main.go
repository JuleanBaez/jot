package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	timestampLayout = "2006-01-02 15:04:05"
	defaultFileName = "/Documents/jot.txt"
	ColorCyan       = "\033[36m"
	ColorReset      = "\033[0m"
)

type Note struct {
	Timestamp string `json:"timestamp"`
	Content   string `json:"content"`
}

func getPath() string {

	// determines the storage location. prioritizes the JOT_PATH and falls back to
	// a default location in the Documents folder.
	path := os.Getenv("JOT_PATH")

	if path == "" {
		homeDir, err := os.UserHomeDir()

		if err != nil {
			fmt.Println("Error getting home directory:", err)
			os.Exit(1)
		}

		path = homeDir + defaultFileName
	}

	return path
}

func userInput(userNote []string) {
	passedNote := strings.Join(userNote, " ")

	formattedTime := time.Now().Format(timestampLayout)
	finalLine := fmt.Sprintf("%s[%s]%s %s\n", ColorCyan, formattedTime, ColorReset, passedNote)

	findFile(finalLine)
}

func findFile(noteText string) {
	filePath := getPath()

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	check(err, "Could not open file")
	defer file.Close()

	_, err = file.WriteString(noteText)
	check(err, "Error writing note")

	fmt.Println("Note Jotted!")
}

func viewNote() {
	file, err := os.Open(getPath())

	if os.IsNotExist(err) {
		fmt.Println("No notes found. Try creating a new note first.")
		return
	}

	check(err, "Failed to open file")
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}
}

func searchNote(searchTerm string) {
	// scans the jot file for lines containing the searchTerm
	// and prints matching lines to the terminal.
	file, err := os.Open(getPath())

	if os.IsNotExist(err) {
		fmt.Println("No notes found. Try creating a new note first.")
		return
	}

	check(err, "Failed to get file")
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {

		currentLine := scanner.Text()

		if strings.Contains(strings.ToLower(currentLine), strings.ToLower(searchTerm)) {
			fmt.Println(currentLine)
		}
	}

	check(scanner.Err(), "Error reading file")
}

func deleteNote(searchTerm string) {
	filePath := getPath()
	tempPath := filePath + ".tmp"

	originalFile, err := os.Open(filePath)

	if os.IsNotExist(err) {
		fmt.Println("No notes found. Try creating a new note first.")
		return
	}

	check(err, "Could not open original file")
	defer originalFile.Close()

	tempFile, err := os.Create(tempPath)
	check(err, "Could not create temp file")

	scanner := bufio.NewScanner(originalFile)

	for scanner.Scan() {
		line := scanner.Text()

		// If the line does not contain the search term, write it to the temp file
		if !strings.Contains(strings.ToLower(line), strings.ToLower(searchTerm)) {
			tempFile.WriteString(line + "\n")
		}
	}

	tempFile.Close()
	originalFile.Close()

	err = os.Rename(tempPath, filePath)
	check(err, "Could not rename temp file")

	fmt.Println("Deleted notes matching:", searchTerm)
}

func tailNote(n int) {
	file, err := os.Open(getPath())

	if os.IsNotExist(err) {
		fmt.Println("No notes found. Try creating a new note first.")
		return
	}

	check(err, "Could not find file")
	defer file.Close()

	buffer := make([]string, n)

	count := 0
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		buffer[count%n] = scanner.Text()
		count++
	}

	check(scanner.Err(), "Error reading file.")

	if count == 0 {
		return
	}

	if count < n {
		for i := 0; i < count; i++ {
			fmt.Println(buffer[i])
		}
	} else {
		startIndex := count % n
		for i := 0; i < n; i++ {
			printIndex := (startIndex + i) % n
			fmt.Println(buffer[(printIndex)])
		}
	}
}

func exportJSON() {
	file, err := os.Open(getPath())

	if os.IsNotExist(err) {
		fmt.Println("No notes found. Try creating a new note first.")
		return
	}

	check(err, "Failed to open file.")
	defer file.Close()

	var exportedNotes []Note

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {

		line := scanner.Text()

		delimiter := "]" + ColorReset + " "

		parts := strings.SplitN(line, delimiter, 2)

		if len(parts) == 2 {
			cleanTimestamp := strings.Replace(parts[0], ColorCyan+"[", "", 1)
			cleanNote := parts[1]

			exportedNotes = append(exportedNotes, Note{
				Timestamp: cleanTimestamp,
				Content:   cleanNote,
			})
		}
	}

	check(scanner.Err(), "Error reading file.")

	jsonData, err := json.MarshalIndent(exportedNotes, "", "  ")
	check(err, "Failed to encode JSON.")

	exportPath := "jot_export.json"

	err = os.WriteFile(exportPath, jsonData, 0644)
	check(err, "Failed to write JSON file.")

	fmt.Printf("Exported %d notes to %s\n", len(exportedNotes), exportPath)
}

func check(err error, message string) {
	if err != nil {
		fmt.Printf("%s: %v\n", message, err)
		os.Exit(1)
	}
}

func main() {
	userNote := os.Args[1:]

	if len(userNote) == 0 {
		fmt.Println("Usage: jot <command> [arguments]")
		os.Exit(1)
	}

	switch userNote[0] {
	case "view":
		viewNote()
	case "search":
		if len(userNote) < 2 {
			fmt.Println("Usage: jot search <search term>")
			os.Exit(1)
		}
		searchTerm := strings.Join(userNote[1:], " ")
		searchNote(searchTerm)
	case "delete":
		if len(userNote) < 2 {
			fmt.Println("Usage: jot delete <search term>")
			os.Exit(1)
		}
		searchTerm := strings.Join(userNote[1:], " ")
		deleteNote(searchTerm)
	case "tail":
		lineCount := 5
		if len(userNote) > 1 {
			parsedNum, err := strconv.Atoi(userNote[1])

			if err == nil && parsedNum > 0 {
				lineCount = parsedNum
			} else {
				fmt.Println("Invalid number: Using default of 5.")
			}
		}
		tailNote(lineCount)
	case "export":
		exportJSON()
	default:
		userInput(userNote)
	}
}
