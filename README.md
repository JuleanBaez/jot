# jot

A terminal-native CLI for K12 IT administrators.
Combines quick note-taking with a full Active Directory automation center,
provisioning accounts, running stale sweeps, triggering ClassLink syncs, and
firing Cortex XDR isolations, all from a single binary.

---

## Installation

**Prerequisites:** Go 1.21+, Tailscale (for AD commands)

```bash
git clone https://github.com/JuleanBaez/jot
cd jot
go build -o jot .
sudo mv jot /usr/local/bin/
```

---

## Notes

```bash
jot <text>            # Save a timestamped note
jot view              # Show all notes
jot search <term>     # Case-insensitive search
jot tail [n]          # Last n notes (default 5)
jot delete <term>     # Remove notes matching term
jot export            # Export all notes to jot_export.json
```

## Tasks

Tasks are auto-pinned to the board and synced to Google Tasks.

```bash
jot task <text>       # Create a task
jot tasks             # List all tasks (pending + done)
jot done <term>       # Mark matching task as done
jot clear             # Remove all completed tasks
```

## Board

```bash
jot pin <text>        # Pin an important note
jot board             # Show all pinned notes and pending tasks
```

---

## Active Directory (`jot ad`)

All AD commands require Tailscale to be connected. The binary performs a 2-second
TCP dial to the AD server before any LDAP operation, if it times out, the command
exits immediately with a clear message rather than hanging.

### Setup

Create `~/.jot/config.json`:

```json
{
  "ad": {
    "server":               "ldaps://100.x.x.x:636",
    "base_dn":              "DC=yourdistrict,DC=org",
    "bind_dn":              "CN=svc-jot,OU=ServiceAccounts,DC=yourdistrict,DC=org",
    "bind_password":        "...",
    "staff_ou":             "OU=Staff",
    "student_ou":           "OU=Students",
    "disabled_ou":          "OU=Disabled",
    "default_groups":       ["Domain Users"],
    "upn_suffix":           "@yourdistrict.net",
    "email_suffix":         "@yourdistrict.com",
    "default_password":     "...",
    "stale_threshold_days": 90
  },
  "classlink": {
    "sync_url": "https://yourdistrict.classlink.com/api/sync",
    "api_key":  "..."
  },
  "cortex_xdr": {
    "script_path": "/opt/scripts/isolate.py"
  },
  "incident_iq": {
    "base_url":          "https://yourdistrict.incidentiq.com",
    "token":             "...",
    "field_first_name":  "Custom_FirstName",
    "field_last_name":   "Custom_LastName",
    "field_role":        "Custom_Role",
    "closed_status_id":  "..."
  },
  "google_oauth": {
    "client_id":     "....apps.googleusercontent.com",
    "project_id":    "your-project-id",
    "client_secret": "..."
  },
  "google_chat": {
    "ops_space":      "spaces/AAAA...",
    "security_space": "spaces/BBBB..."
  }
}
```

> `ldaps://` is required. The `unicodePwd` attribute (used to set passwords at
> account creation) is rejected by Active Directory over plain LDAP.

### Commands

```bash
jot ad new <first> <last> <staff|student> [dept|grade]
```
Provisions a new account end-to-end:
- Collision-safe username (`jdoe`, `jdoe1`, ...)
- Routes to `OU=Staff` or `OU=Students` based on role
- Sets `mail` attribute for GCDS → Google Workspace sync
- Assigns default groups
- Triggers ClassLink roster sync
- Posts provisioning summary (including temp password) to Google Chat ops space

```bash
jot ad sync
```
Polls Incident IQ for open onboarding tickets, runs `ad new` for each one using a
single shared LDAP connection, then closes each ticket.

```bash
jot ad clean
```
Finds accounts inactive beyond `stale_threshold_days` (default 90), disables them,
moves them to `OU=Disabled`, triggers a ClassLink sync, and posts a report to Chat.

```bash
jot ad unlock <username>    # Clear account lockout (lockoutTime = 0)
jot ad reset  <username>    # Force password change at next login (pwdLastSet = 0)
```

```bash
jot ad isolate <username>
```
Incident response nuclear option — runs three steps in sequence:
1. Disables the AD account
2. Executes `python3 <cortex_xdr.script_path> --user <username>`
3. Posts a security alert to the Google Chat security space

```bash
jot ad audit [group]        # Group membership CSV report → Chat summary
jot ad backup               # gzip JSON snapshot of all AD users → ~/.jot/backups/
jot ad help                 # Full AD command reference
```

---

## Google Integrations

### Google Tasks (`jot auth`)

Requires `~/.jot/credentials.json` (OAuth Desktop App credentials from Google Cloud Console
with the Tasks API enabled).

```bash
jot auth    # One-time browser auth flow; token cached to ~/.jot/token.json
```

After auth, every `jot task` syncs to your default Google Tasks list automatically.

### Google Chat (AD commands)

Chat notifications use the Google Chat API with OAuth (not webhooks). Credentials
come from the `google_oauth` block in `config.json`. Token is cached at
`~/.jot/chat_token.json` and refreshed silently on each run.

On first use of any AD command that sends a Chat message, a browser window opens
for a one-time authorization. All subsequent runs are silent.

### GCDS Completion Loop (Apps Script)

Because GCDS provisioning is asynchronous, a Google Apps Script daemon
(`gcds_notifier.gs`) polls the Workspace Admin Audit log every 15 minutes for
`CREATE_USER` events and fires a formatted Chat card when each account goes live.

See `gcds_notifier.gs` for setup instructions.

---

## Environment

| Variable   | Default                  | Purpose                        |
|------------|--------------------------|--------------------------------|
| `JOT_PATH` | `~/Documents/jot.txt`    | Override notes file location   |

| Path                        | Purpose                              |
|-----------------------------|--------------------------------------|
| `~/.jot/config.json`        | AD, ClassLink, IIQ, Chat, OAuth      |
| `~/.jot/credentials.json`   | Google Tasks OAuth client creds      |
| `~/.jot/token.json`         | Google Tasks cached token            |
| `~/.jot/chat_token.json`    | Google Chat cached token             |
| `~/.jot/backups/`           | AD backup archives                   |
| `~/.jot/audits/`            | Group audit CSV reports              |
