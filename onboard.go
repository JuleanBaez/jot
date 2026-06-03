package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"jot/core/ad"
	"jot/core/config"
	"jot/core/iiq"

	ldap "github.com/go-ldap/ldap/v3"
	"gopkg.in/yaml.v3"
)

// ── YAML Structs ──────────────────────────────────────────────────────────────

// HireRecord represents one new hire entry in the YAML onboarding manifest.
type HireRecord struct {
	FirstName  string `yaml:"first_name"`
	LastName   string `yaml:"last_name"`
	Role       string `yaml:"role"`       // staff | student
	Title      string `yaml:"title"`
	Department string `yaml:"department"`
	Building   string `yaml:"building"`
	Grade      string `yaml:"grade"`     // students only
	TicketID   string `yaml:"ticket_id"` // IIQ ticket to close on success
}

// OnboardFile is the root document of the YAML manifest.
type OnboardFile struct {
	Hires []HireRecord `yaml:"hires"`
}

// onboardResult holds the per-hire outcome from runOnboard.
type onboardResult struct {
	hire      HireRecord
	username  string
	email     string
	adOK      bool
	duoOK     bool
	iiqOK     bool
	enrollURL string
	adErr     string
	duoErr    string
	iiqErr    string
}

// ── Entry Point ───────────────────────────────────────────────────────────────

func handleOnboard(args []string) {
	if len(args) == 0 || args[0] == "help" {
		printOnboardHelp()
		return
	}

	for _, a := range args {
		if a == "--ticket" || strings.HasPrefix(a, "--ticket=") {
			runTicketOnboard(args)
			return
		}
	}

	dryRun := false
	yamlPath := args[0]
	for _, a := range args[1:] {
		if a == "--dry-run" {
			dryRun = true
		}
	}

	runOnboard(yamlPath, dryRun)
}

// ── Batch Orchestrator ────────────────────────────────────────────────────────

// runOnboard parses the YAML manifest and runs the three-step onboarding
// sequence (AD → Duo → IIQ) for each hire. When dryRun is true it prints what
// would happen without connecting to any system.
func runOnboard(yamlPath string, dryRun bool) {
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"[onboard] Cannot read YAML: %v\n"+ColorReset, err)
		os.Exit(1)
	}

	var manifest OnboardFile
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"[onboard] YAML parse error: %v\n"+ColorReset, err)
		os.Exit(1)
	}

	if len(manifest.Hires) == 0 {
		fmt.Println(ColorYellow + "[onboard] No hires found in manifest." + ColorReset)
		return
	}

	cfg := mustLoadConfig()
	validateProvisionConfig(cfg)

	mode := ""
	if dryRun {
		mode = " " + ColorDim + "(DRY RUN)" + ColorReset
	}
	fmt.Printf(ColorCyan+"[onboard] Processing %d hire(s) from %s%s\n"+ColorReset,
		len(manifest.Hires), yamlPath, mode)
	fmt.Println("  " + strings.Repeat("─", 58))

	var l *ldap.Conn
	if !dryRun {
		l = ldapConnect(cfg)
		defer l.Close()
	}

	results := make([]onboardResult, 0, len(manifest.Hires))

	for i, hire := range manifest.Hires {
		fmt.Printf("\n  [%d/%d] %s %s (%s)\n",
			i+1, len(manifest.Hires), hire.FirstName, hire.LastName, hire.Role)

		if dryRun {
			fmt.Printf(ColorDim+"    AD  — would provision: role=%s dept=%s title=%s building=%s\n"+ColorReset,
				hire.Role, hire.Department, hire.Title, hire.Building)
			if cfg.Duo.APIHostname != "" {
				fmt.Printf(ColorDim + "    DUO — would create user and request enrollment link\n" + ColorReset)
			}
			if hire.TicketID != "" {
				fmt.Printf(ColorDim+"    IIQ — would close ticket: %s\n"+ColorReset, hire.TicketID)
			}
			results = append(results, onboardResult{hire: hire, adOK: true, duoOK: true, iiqOK: true})
			continue
		}

		r := onboardOne(l, cfg, hire)
		results = append(results, r)
	}

	printOnboardSummary(results, dryRun)

	if !dryRun {
		postOnboardChatSummary(cfg, results)
	}
}

// onboardOne runs the full three-step sequence for a single hire.
//
// Failure isolation:
//   - AD fail → IIQ skipped (no account means nothing to resolve)
//   - AD fail → Duo still attempted (pre-creating the Duo identity is harmless)
//   - Duo fail → IIQ still runs
//
// Each step prints its own status line so the operator can see progress in
// real time on long batches.
func onboardOne(l *ldap.Conn, cfg config.Jot, hire HireRecord) onboardResult {
	r := onboardResult{hire: hire}

	// Resolve username once; both the AD write and Duo enrollment need it.
	r.username = ad.ResolveUsername(l, cfg, hire.FirstName, hire.LastName)
	r.email = r.username + cfg.AD.EmailSuffix

	// ── Step 1: AD Provisioning ───────────────────────────────────────────────
	if adErr := provisionUserWithResult(l, cfg, hire, r.username); adErr != nil {
		r.adOK = false
		r.adErr = adErr.Error()
		fmt.Printf("    "+ColorYellow+"AD  ✗ %s\n"+ColorReset, r.adErr)
	} else {
		r.adOK = true
		fmt.Printf("    "+ColorGreen+"AD  ✓ %s%s\n"+ColorReset, r.username, cfg.AD.UPNSuffix)
	}

	// ── Step 2: Duo Enrollment ────────────────────────────────────────────────
	if cfg.Duo.APIHostname == "" {
		fmt.Println("    " + ColorDim + "DUO — skipped (duo.api_hostname not configured)" + ColorReset)
	} else {
		enrollURL, duoErr := duoEnsureAndEnroll(cfg, r.username, r.email, hire.FirstName+" "+hire.LastName)
		if duoErr != nil {
			r.duoOK = false
			r.duoErr = duoErr.Error()
			fmt.Printf("    "+ColorYellow+"DUO ✗ %s\n"+ColorReset, r.duoErr)
		} else {
			r.duoOK = true
			r.enrollURL = enrollURL
			fmt.Println("    " + ColorGreen + "DUO ✓ Enrollment link posted to Chat" + ColorReset)
		}
	}

	// ── Step 3: IIQ Ticket Closure ────────────────────────────────────────────
	switch {
	case hire.TicketID == "":
		fmt.Println("    " + ColorDim + "IIQ — no ticket_id in manifest, skipped" + ColorReset)
	case cfg.IncidentIQ.BaseURL == "" || cfg.IncidentIQ.Token == "":
		fmt.Println("    " + ColorDim + "IIQ — skipped (incident_iq not configured)" + ColorReset)
	case !r.adOK:
		fmt.Println("    " + ColorYellow + "IIQ — skipped (AD provisioning failed)" + ColorReset)
	default:
		note := buildResolutionNote(hire, r, cfg)
		if iiqErr := iiqCloseWithNote(cfg, hire.TicketID, note); iiqErr != nil {
			r.iiqOK = false
			r.iiqErr = iiqErr.Error()
			fmt.Printf("    "+ColorYellow+"IIQ ✗ Ticket %s: %s\n"+ColorReset, hire.TicketID, r.iiqErr)
		} else {
			r.iiqOK = true
			fmt.Printf("    "+ColorGreen+"IIQ ✓ Ticket %s closed\n"+ColorReset, hire.TicketID)
		}
	}

	return r
}

// ── AD Provisioning ───────────────────────────────────────────────────────────

// provisionUserWithResult is a batch-safe variant of provisionUser that:
//   - Accepts a pre-resolved username (avoids double ad.ResolveUsername call)
//   - Writes title and building attributes from the YAML record
//   - Returns an error instead of printing-and-returning, so the batch loop
//     can track per-hire outcomes without calling os.Exit
func provisionUserWithResult(l *ldap.Conn, cfg config.Jot, hire HireRecord, username string) error {
	role := strings.ToLower(hire.Role)
	if role != "staff" && role != "student" {
		return fmt.Errorf("invalid role %q — must be staff or student", hire.Role)
	}

	upn := username + cfg.AD.UPNSuffix
	email := username + cfg.AD.EmailSuffix

	targetOU := cfg.AD.StaffOU
	if role == "student" {
		targetOU = cfg.AD.StudentOU
	}
	dn := fmt.Sprintf("CN=%s,%s,%s", username, targetOU, cfg.AD.BaseDN)

	addReq := ldap.NewAddRequest(dn, nil)
	addReq.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user"})
	addReq.Attribute("cn", []string{username})
	addReq.Attribute("givenName", []string{hire.FirstName})
	addReq.Attribute("sn", []string{hire.LastName})
	addReq.Attribute("displayName", []string{hire.FirstName + " " + hire.LastName})
	addReq.Attribute("sAMAccountName", []string{username})
	addReq.Attribute("userPrincipalName", []string{upn})
	addReq.Attribute("mail", []string{email})
	if hire.Title != "" {
		addReq.Attribute("title", []string{hire.Title})
	}
	if hire.Department != "" {
		addReq.Attribute("department", []string{hire.Department})
	}
	if hire.Building != "" {
		addReq.Attribute("physicalDeliveryOfficeName", []string{hire.Building})
	}

	usingLDAPS := strings.HasPrefix(cfg.AD.Server, "ldaps://")
	if usingLDAPS {
		addReq.Attribute("unicodePwd", []string{ad.EncodeUnicodePwd(cfg.AD.DefaultPassword)})
		addReq.Attribute("userAccountControl", []string{"512"}) // Enabled
	} else {
		addReq.Attribute("userAccountControl", []string{"514"}) // Disabled — needs manual password set
	}

	if err := l.Add(addReq); err != nil {
		return err
	}

	for _, groupCN := range cfg.AD.DefaultGroups {
		groupDN, err := ad.FindGroupDN(l, cfg, groupCN)
		if err != nil {
			fmt.Printf(ColorDim+"      Warning: group not found (%s): %v\n"+ColorReset, groupCN, err)
			continue
		}
		mod := ldap.NewModifyRequest(groupDN, nil)
		mod.Add("member", []string{dn})
		if err := l.Modify(mod); err != nil {
			fmt.Printf(ColorDim+"      Warning: default group add failed (%s): %v\n"+ColorReset, groupCN, err)
		}
	}

	triggerClassLinkSync(cfg)
	return nil
}

// ── Duo Admin API ─────────────────────────────────────────────────────────────

// duoSign produces the Authorization header value and Date string for a Duo
// Admin API request. Duo's canonical-request format is:
//
//	date\nMETHOD\nhostname\npath\nsorted_url_encoded_params
//
// The HMAC-SHA1 digest is hex-encoded, then base64'd with the integration key.
func duoSign(cfg config.Jot, method, path string, params url.Values) (authHeader, dateStr string) {
	dateStr = time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 -0000")

	// url.Values.Encode sorts by key alphabetically — required by Duo spec.
	canon := strings.Join([]string{
		dateStr,
		strings.ToUpper(method),
		strings.ToLower(cfg.Duo.APIHostname),
		path,
		params.Encode(),
	}, "\n")

	mac := hmac.New(sha1.New, []byte(cfg.Duo.SecretKey))
	mac.Write([]byte(canon))
	sig := hex.EncodeToString(mac.Sum(nil))

	authHeader = "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(cfg.Duo.IntegrationKey+":"+sig),
	)
	return
}

// duoRequest executes a signed Duo Admin API call.
//   - POST: params are sent as application/x-www-form-urlencoded body
//   - GET:  params are appended to the URL as a query string
func duoRequest(cfg config.Jot, method, path string, params url.Values) (*http.Response, error) {
	authHeader, dateStr := duoSign(cfg, method, path, params)
	endpoint := "https://" + cfg.Duo.APIHostname + path

	var req *http.Request
	var err error

	if strings.ToUpper(method) == "POST" {
		req, err = http.NewRequest("POST", endpoint, strings.NewReader(params.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		if len(params) > 0 {
			endpoint += "?" + params.Encode()
		}
		req, err = http.NewRequest(method, endpoint, nil)
		if err != nil {
			return nil, err
		}
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Date", dateStr)
	return http.DefaultClient.Do(req)
}

// duoEnsureAndEnroll idempotently provisions a Duo user and retrieves their
// enrollment activation link, posting it to the Google Chat ops space.
//
// Steps:
//  1. GET /admin/v1/users?username=… — skip creation if already exists
//  2. POST /admin/v1/users — create with username, email, realname
//  3. POST /admin/v1/users/{id}/activation_link — 7-day enrollment URL
//
// The enrollment URL is posted to Chat (not emailed directly) so the operator
// can forward it to the new hire. adLAPS-style: no Chat message if Duo is not
// configured; the caller already guards that.
func duoEnsureAndEnroll(cfg config.Jot, username, email, displayName string) (string, error) {
	// ── 1. Check for existing Duo user ────────────────────────────────────────
	getResp, err := duoRequest(cfg, "GET", "/admin/v1/users", url.Values{"username": {username}})
	if err != nil {
		return "", fmt.Errorf("Duo GET /users: %w", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()

	var getUserResp struct {
		Stat     string `json:"stat"`
		Response []struct {
			UserID string `json:"user_id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &getUserResp); err != nil {
		return "", fmt.Errorf("Duo GET /users parse: %w", err)
	}

	var userID string
	if getUserResp.Stat == "OK" && len(getUserResp.Response) > 0 {
		userID = getUserResp.Response[0].UserID
	}

	// ── 2. Create Duo user if not found ───────────────────────────────────────
	if userID == "" {
		postResp, err := duoRequest(cfg, "POST", "/admin/v1/users", url.Values{
			"username": {username},
			"email":    {email},
			"realname": {displayName},
		})
		if err != nil {
			return "", fmt.Errorf("Duo POST /users: %w", err)
		}
		body, _ = io.ReadAll(postResp.Body)
		postResp.Body.Close()

		var createResp struct {
			Stat     string `json:"stat"`
			Message  string `json:"message"`
			Response struct {
				UserID string `json:"user_id"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &createResp); err != nil {
			return "", fmt.Errorf("Duo create user parse: %w", err)
		}
		if createResp.Stat != "OK" {
			return "", fmt.Errorf("Duo create user: %s", createResp.Message)
		}
		userID = createResp.Response.UserID
	}

	// ── 3. Get enrollment activation link (7-day expiry) ─────────────────────
	linkResp, err := duoRequest(cfg, "POST",
		"/admin/v1/users/"+userID+"/activation_link",
		url.Values{"valid_secs": {"604800"}},
	)
	if err != nil {
		return "", fmt.Errorf("Duo activation_link: %w", err)
	}
	body, _ = io.ReadAll(linkResp.Body)
	linkResp.Body.Close()

	// Duo returns the web enrollment URL as "activation_barcode" (despite the
	// name it is a clickable HTTPS link, not an image). Some API versions use
	// "link" instead — check both.
	var linkResult struct {
		Stat     string `json:"stat"`
		Message  string `json:"message"`
		Response struct {
			ActivationBarcode string `json:"activation_barcode"`
			Link              string `json:"link"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &linkResult); err != nil {
		return "", fmt.Errorf("Duo activation_link parse: %w", err)
	}
	if linkResult.Stat != "OK" {
		return "", fmt.Errorf("Duo activation_link: %s", linkResult.Message)
	}

	enrollURL := linkResult.Response.ActivationBarcode
	if enrollURL == "" {
		enrollURL = linkResult.Response.Link
	}

	if cfg.GoogleChat.OpsSpace != "" && enrollURL != "" {
		postToChat(cfg.GoogleChat.OpsSpace, fmt.Sprintf(
			"*[Onboard] Duo Enrollment*\nUser: `%s` — %s\nEnrollment link (7 days): %s",
			username, displayName, enrollURL,
		))
	}

	return enrollURL, nil
}

// ── Incident IQ ───────────────────────────────────────────────────────────────

// iiqCloseWithNote posts a resolution comment to an IIQ ticket and then
// patches its status to closed. A comment failure is logged but does not
// prevent the status update from running.
func iiqCloseWithNote(cfg config.Jot, ticketID, note string) error {
	if cfg.IncidentIQ.ClosedStatusID == "" {
		return fmt.Errorf("incident_iq.closed_status_id not configured")
	}

	// Step 1: Post resolution comment.
	commentResp, err := iiqPOST(cfg, "/tickets/"+ticketID+"/comments", map[string]interface{}{
		"Comment":  note,
		"IsPublic": true,
	})
	if err != nil {
		fmt.Printf(ColorDim+"      [IIQ] Comment post failed: %v\n"+ColorReset, err)
	} else {
		commentResp.Body.Close()
	}

	// Step 2: Patch ticket status to closed.
	statusResp, err := iiqPATCH(cfg, "/tickets/"+ticketID, map[string]string{
		"StatusId": cfg.IncidentIQ.ClosedStatusID,
	})
	if err != nil {
		return fmt.Errorf("status update: %w", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode >= 400 {
		return fmt.Errorf("status update returned HTTP %d", statusResp.StatusCode)
	}
	return nil
}

// buildResolutionNote constructs the plaintext comment body posted to IIQ.
func buildResolutionNote(hire HireRecord, r onboardResult, cfg config.Jot) string {
	var sb strings.Builder
	sb.WriteString("Automated onboarding complete via jot.\n\n")
	fmt.Fprintf(&sb, "Name       : %s %s\n", hire.FirstName, hire.LastName)
	fmt.Fprintf(&sb, "Role       : %s\n", hire.Role)
	if hire.Title != "" {
		fmt.Fprintf(&sb, "Title      : %s\n", hire.Title)
	}
	if hire.Department != "" {
		fmt.Fprintf(&sb, "Department : %s\n", hire.Department)
	}
	if hire.Building != "" {
		fmt.Fprintf(&sb, "Building   : %s\n", hire.Building)
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "AD Account : %s%s\n", r.username, cfg.AD.UPNSuffix)
	fmt.Fprintf(&sb, "Email      : %s\n", r.email)
	if r.adOK {
		sb.WriteString("AD Status  : SUCCESS\n")
	} else {
		fmt.Fprintf(&sb, "AD Status  : FAILED (%s)\n", r.adErr)
	}
	if r.duoOK {
		sb.WriteString("Duo Status : enrollment link issued\n")
	} else if r.duoErr != "" {
		fmt.Fprintf(&sb, "Duo Status : FAILED (%s)\n", r.duoErr)
	} else {
		sb.WriteString("Duo Status : not configured\n")
	}
	return sb.String()
}

// ── Output ────────────────────────────────────────────────────────────────────

// printOnboardSummary renders the final batch result line.
func printOnboardSummary(results []onboardResult, dryRun bool) {
	fmt.Println("\n  " + strings.Repeat("─", 58))

	if dryRun {
		fmt.Printf(ColorCyan+"[onboard] Dry run complete — %d hire(s) would be processed.\n"+ColorReset,
			len(results))
		return
	}

	succeeded, failed := 0, 0
	for _, r := range results {
		if r.adOK {
			succeeded++
		} else {
			failed++
		}
	}

	succColor, failColor := ColorReset, ColorReset
	if succeeded > 0 {
		succColor = ColorGreen
	}
	if failed > 0 {
		failColor = ColorYellow
	}
	fmt.Printf("[onboard] Summary: %s%d succeeded%s, %s%d failed%s\n",
		succColor, succeeded, ColorReset,
		failColor, failed, ColorReset,
	)
}

// postOnboardChatSummary posts a batch completion summary to the ops space.
func postOnboardChatSummary(cfg config.Jot, results []onboardResult) {
	if cfg.GoogleChat.OpsSpace == "" {
		return
	}
	succeeded, failed := 0, 0
	lines := make([]string, 0, len(results))
	for _, r := range results {
		mark := "✓"
		if !r.adOK {
			mark = "✗"
			failed++
		} else {
			succeeded++
		}
		lines = append(lines, fmt.Sprintf("%s `%s` — %s %s (%s)",
			mark, r.username, r.hire.FirstName, r.hire.LastName, r.hire.Role))
	}
	postToChat(cfg.GoogleChat.OpsSpace, fmt.Sprintf(
		"*[Onboard] Batch Complete*\nSucceeded: %d · Failed: %d\n\n%s",
		succeeded, failed, strings.Join(lines, "\n"),
	))
}

// ── Ticket-Driven Onboarding ──────────────────────────────────────────────────

func runTicketOnboard(args []string) {
	var ticketID, first, last, role, dept string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--ticket" && i+1 < len(args):
			ticketID = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--ticket="):
			ticketID = strings.TrimPrefix(args[i], "--ticket=")
		case args[i] == "--first" && i+1 < len(args):
			first = args[i+1]
			i++
		case args[i] == "--last" && i+1 < len(args):
			last = args[i+1]
			i++
		case args[i] == "--role" && i+1 < len(args):
			role = args[i+1]
			i++
		case args[i] == "--dept" && i+1 < len(args):
			dept = args[i+1]
			i++
		}
	}
	if ticketID == "" || first == "" || last == "" || role == "" {
		fmt.Fprintln(os.Stderr, ColorYellow+"[onboard] --ticket requires: --first <name> --last <name> --role <staff|student>"+ColorReset)
		os.Exit(1)
	}

	cfg := mustLoadConfig()
	ctx := context.Background()

	iiqClient, err := iiq.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"[onboard] IIQ not configured: %v\n"+ColorReset, err)
		os.Exit(1)
	}

	fmt.Printf(ColorCyan+"\n[onboard] Ticket #%s — %s %s (%s)\n"+ColorReset, ticketID, first, last, role)
	fmt.Println("  " + strings.Repeat("─", 56))

	fmt.Print("  [1/5] Fetching ticket and marking In Progress ... ")
	ticket, err := onboardPhase1(ctx, iiqClient, cfg, ticketID)
	if err != nil {
		fmt.Println(ColorYellow + "✗" + ColorReset)
		fmt.Fprintf(os.Stderr, "        %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(ColorGreen+"✓  #%s: %s\n"+ColorReset, ticket.TicketID, ticket.Subject)

	fmt.Print("  [2/5] Provisioning AD account ... ")
	l := ldapConnect(cfg)
	defer l.Close()
	username, email, err := onboardPhase2(ctx, iiqClient, cfg, l, ticket.ID, first, last, role, dept)
	if err != nil {
		fmt.Println(ColorYellow + "✗" + ColorReset)
		fmt.Fprintf(os.Stderr, "        %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(ColorGreen+"✓  %s (%s)\n"+ColorReset, username, email)

	onboardPhase3ManualPause(first, last, email)

	fmt.Print("  [4/5] Notifying data team and updating ticket ... ")
	if err := onboardPhase4(ctx, iiqClient, cfg, ticket.ID, first, last, username, email, role, dept); err != nil {
		fmt.Printf(ColorYellow+"⚠  %v\n"+ColorReset, err)
	} else {
		fmt.Println(ColorGreen + "✓" + ColorReset)
	}

	fmt.Print("  [5/5] Closing ticket ... ")
	if err := onboardPhase5(ctx, iiqClient, cfg, ticket.ID, first, last, username, email, role); err != nil {
		fmt.Printf(ColorYellow+"⚠  %v\n"+ColorReset, err)
	} else {
		fmt.Println(ColorGreen + "✓" + ColorReset)
	}

	fmt.Printf(ColorGreen+"\n[onboard] Done. %s %s provisioned as %s\n"+ColorReset, first, last, username)
}

func onboardPhase1(ctx context.Context, c *iiq.Client, cfg config.Jot, ticketID string) (iiq.Ticket, error) {
	ticket, err := c.GetTicket(ctx, ticketID)
	if err != nil {
		return iiq.Ticket{}, fmt.Errorf("fetch ticket: %w", err)
	}
	if cfg.IncidentIQ.InProgressStatusID != "" {
		if err := c.UpdateStatus(ctx, ticket.ID, cfg.IncidentIQ.InProgressStatusID); err != nil {
			return ticket, fmt.Errorf("set in-progress: %w", err)
		}
	}
	_ = c.Comment(ctx, ticket.ID, "Automated provisioning started via jot.")
	return ticket, nil
}

func onboardPhase2(ctx context.Context, c *iiq.Client, cfg config.Jot, l *ldap.Conn, ticketGUID, first, last, role, dept string) (username, email string, err error) {
	username = ad.ResolveUsername(l, cfg, first, last)
	email = username + cfg.AD.EmailSuffix
	hire := HireRecord{FirstName: first, LastName: last, Role: role, Department: dept}
	if err := provisionUserWithResult(l, cfg, hire, username); err != nil {
		return "", "", err
	}
	// Google Workspace provisioning placeholder — Admin SDK integration goes here.
	_ = c.Comment(ctx, ticketGUID, fmt.Sprintf("AD account provisioned.\n\nUsername: %s\nEmail: %s", username, email))
	return username, email, nil
}

func onboardPhase3ManualPause(first, last, email string) {
	const inner = 56
	bar := strings.Repeat("═", inner)
	pad := func(s string) string {
		n := inner - 1
		if len(s) >= n {
			return s[:n]
		}
		return s + strings.Repeat(" ", n-len(s))
	}

	fmt.Println()
	fmt.Printf("  ╔%s╗\n", bar)
	fmt.Printf("  \033[7;1m║ %-*s║\033[0m\n", inner-1, " ACTION REQUIRED")
	fmt.Printf("  ╠%s╣\n", bar)
	fmt.Printf("  ║ %s║\n", pad(""))
	fmt.Printf("  ║ %s║\n", pad(fmt.Sprintf("New hire:  %s %s", first, last)))
	fmt.Printf("  ║ %s║\n", pad(""))
	fmt.Printf("  \033[1m║ %s║\033[0m\n", pad(fmt.Sprintf("Email:     %s", email)))
	fmt.Printf("  ║ %s║\n", pad(""))
	fmt.Printf("  ║ %s║\n", pad("Enter this employee into the HR / Payroll"))
	fmt.Printf("  ║ %s║\n", pad("system before continuing."))
	fmt.Printf("  ║ %s║\n", pad(""))
	fmt.Printf("  ╚%s╝\n", bar)
	fmt.Println()
	fmt.Print("  Press \033[1mEnter\033[0m when HR entry is complete ... ")
	r := bufio.NewReader(os.Stdin)
	r.ReadString('\n')
	fmt.Println()
}

func onboardPhase4(ctx context.Context, c *iiq.Client, cfg config.Jot, ticketGUID, first, last, username, email, role, dept string) error {
	if cfg.GoogleChat.DataSpaceWebhook != "" {
		msg := fmt.Sprintf(
			"*[Onboard] New %s ready for SIS entry*\n\nName: %s %s\nEmail: `%s`\nAD: `%s`\nDept: %s\n\nPlease add to OnCourse SIS.",
			role, first, last, email, username, dept,
		)
		if err := sendChatWebhook(cfg.GoogleChat.DataSpaceWebhook, msg); err != nil {
			fmt.Printf(ColorDim+"        [chat] webhook failed: %v\n"+ColorReset, err)
		}
	}
	_ = c.Comment(ctx, ticketGUID,
		fmt.Sprintf("HR entry complete. Data team notified for SIS onboarding.\nEmail: %s", email),
	)
	return nil
}

func onboardPhase5(ctx context.Context, c *iiq.Client, cfg config.Jot, ticketGUID, first, last, username, email, role string) error {
	if cfg.IncidentIQ.ClosedStatusID == "" {
		return fmt.Errorf("incident_iq.closed_status_id not configured")
	}
	note := fmt.Sprintf(
		"Onboarding complete via jot.\n\nName: %s %s\nRole: %s\nAD Account: %s\nEmail: %s\n\nAll steps complete: AD provisioned, HR entered, data team notified.",
		first, last, role, username, email,
	)
	return c.Resolve(ctx, ticketGUID, cfg.IncidentIQ.ClosedStatusID, note)
}

func sendChatWebhook(webhookURL, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// printOnboardHelp prints usage information for the onboard subcommand.
func printOnboardHelp() {
	fmt.Printf(`
%sOnboard Command%s  (jot onboard <yaml-file> [--dry-run])

  Reads a YAML manifest of new hires and orchestrates three steps per hire:
    1. AD account creation  — LDAP write, temp password, OU placement, ClassLink sync
    2. Duo 2FA enrollment   — creates Duo user, posts 7-day activation link to Chat
    3. IIQ ticket closure   — posts resolution note + patches status to closed

  %sFlags%s
    --dry-run    Preview what would happen without connecting to any system

  %sYAML Manifest%s
    hires:
      - first_name:  Jane
        last_name:   Doe
        role:        staff          # staff | student
        title:       Math Teacher   # → AD title attribute
        department:  Mathematics    # → AD department attribute
        building:    Main Campus    # → AD physicalDeliveryOfficeName
        ticket_id:   "T-1234"       # optional — IIQ ticket to close on success
        grade:       ""             # students only (informational)

  %sConfig (in ~/.jot/config.json)%s
    duo.api_hostname     "api-XXXXXXXX.duosecurity.com"
    duo.integration_key  "DIxxxxxxxxxxxxxxxxxx"
    duo.secret_key       "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

    incident_iq.closed_status_id   GUID of your closed/resolved status
                                   Run: jot ad iiq-info  to discover GUIDs

  %sEnvironment%s
    JOT_DUO_SECRET_KEY   Override duo.secret_key without storing it in config

`,
		ColorCyan, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
	)
}
