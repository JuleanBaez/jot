# Jot 

A terminal-native note-taking utility written in Go. 

## Features

* **Capture:** Type `jot <your note>` and it's instantly saved with a localized timestamp.
* **Colorized Output:** Uses ANSI escape codes for clean, readable timestamps in the terminal.
* **Portable Storage:** Notes are saved to `~/Documents/jot.txt` by default, but the storage location can be dynamically overridden using environment variables.
* **Atomic Deletions:** Safely removes specific notes using temporary file staging and atomic renaming to prevent data corruption.
* **Case-Insensitive Search:** Quickly filter through hundreds of notes using the built-in search router.

## Installation

Ensure you have [Go installed](https://go.dev/doc/install) on your system.

**Using `go install`**
```bash
go install [github.com/yourusername/jot@latest](https://github.com/yourusername/jot@latest)