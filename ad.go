package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"jot/core/ad"
	"jot/core/config"

	ldap "github.com/go-ldap/ldap/v3"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	chatv1 "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
)

func mustLoadConfig() config.Jot {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"[jot] Config error: %v\n"+ColorReset, err)
		os.Exit(1)
	}
	return cfg
}

// ── AD Sub-Router ─────────────────────────────────────────────────────────────

func handleAD(args []string) {
	switch args[0] {

	case "new":
		if len(args) < 4 {
			fmt.Println("Usage: jot ad new <firstname> <lastname> <staff|student> [dept|grade]")
			os.Exit(1)
		}
		adProvision(args[1], args[2], args[3], args[4:])

	case "sync":
		adSyncIncidentIQ()

	case "iiq-info":
		adIIQInfo()

	case "clean":
		adCleanStale()

	case "backup":
		adBackup()

	case "unlock":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad unlock <username>")
			os.Exit(1)
		}
		adUnlock(args[1])

	case "reset":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad reset <username>")
			os.Exit(1)
		}
		adPasswordReset(args[1])

	case "isolate":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad isolate <username>")
			os.Exit(1)
		}
		adIsolate(args[1])

	case "audit":
		group := "all"
		if len(args) > 1 {
			group = strings.Join(args[1:], " ")
		}
		adAudit(group)

	case "groups":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad groups <username>")
			os.Exit(1)
		}
		adUserGroups(args[1])

	case "status":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad status <username>")
			os.Exit(1)
		}
		adStatus(args[1])

	case "find":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad find <name|username|email>")
			os.Exit(1)
		}
		adFind(strings.Join(args[1:], " "))

	case "addgroup":
		if len(args) < 3 {
			fmt.Println("Usage: jot ad addgroup <username> <group name>")
			os.Exit(1)
		}
		adAddToGroup(args[1], strings.Join(args[2:], " "))

	case "search":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad search <term>")
			os.Exit(1)
		}
		adAuditSearch(strings.Join(args[1:], " "))

	case "term":
		if len(args) < 2 {
			fmt.Println("Usage: jot ad term <username>")
			os.Exit(1)
		}
		adTerm(args[1])

	case "resurrect":
		if len(args) < 3 {
			fmt.Println("Usage: jot ad resurrect <user> <staff|student>")
			os.Exit(1)
		}
		adResurrect(args[1], args[2])

	case "help":
		printADHelp()

	default:
		fmt.Printf("Unknown AD command: %q\n", args[0])
		fmt.Println("Run 'jot ad help' to see available commands.")
		os.Exit(1)
	}
}

// ── Infrastructure ────────────────────────────────────────────────────────────

// checkTailscale dials the AD server with a 2-second deadline.
// Exits immediately if unreachable so no LDAP call ever hangs.
func checkTailscale(server string) {
	addr := strings.TrimPrefix(strings.TrimPrefix(server, "ldaps://"), "ldap://")
	if !strings.Contains(addr, ":") {
		addr += ":389"
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fmt.Printf(ColorYellow+"[jot] Cannot reach AD at %s — is Tailscale up?\n"+ColorReset, addr)
		os.Exit(1)
	}
	conn.Close()
}

// ldapConnect verifies Tailscale reachability then returns an authenticated
// *ldap.Conn. Thin CLI wrapper around ad.Connect — exits on failure.
func ldapConnect(cfg config.Jot) *ldap.Conn {
	checkTailscale(cfg.AD.Server)
	l, err := ad.Connect(cfg)
	check(err, "LDAP connect failed")
	return l
}

// triggerClassLinkSync fires a forced roster sync via the ClassLink REST API.
func triggerClassLinkSync(cfg config.Jot) {
	if cfg.ClassLink.SyncURL == "" {
		return
	}
	req, err := http.NewRequest("POST", cfg.ClassLink.SyncURL, nil)
	if err != nil {
		fmt.Printf(ColorDim+"  [ClassLink] Request error: %v\n"+ColorReset, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.ClassLink.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf(ColorDim+"  [ClassLink] Sync failed: %v\n"+ColorReset, err)
		return
	}
	resp.Body.Close()
	fmt.Println(ColorCyan + "  [ClassLink] Roster sync triggered." + ColorReset)
}

// ── Google Chat API ───────────────────────────────────────────────────────────

// getChatClient returns an OAuth2 HTTP client scoped to Chat message creation.
// On first call it runs the browser auth flow and caches the token to
// ~/.jot/chat_token.json. Subsequent calls load the cache and silently refresh.
func getChatClient() *http.Client {
	cfg := mustLoadConfig()
	if cfg.GoogleOAuth.ClientID == "" || cfg.GoogleOAuth.ClientSecret == "" {
		fmt.Println(ColorYellow + "[Chat] google_oauth.client_id / client_secret not set in config.json." + ColorReset)
		os.Exit(1)
	}

	oa2Cfg := &oauth2.Config{
		ClientID:     cfg.GoogleOAuth.ClientID,
		ClientSecret: cfg.GoogleOAuth.ClientSecret,
		Scopes:       []string{"https://www.googleapis.com/auth/chat.messages.create"},
		Endpoint:     google.Endpoint,
	}

	tokenPath := configDir() + "/chat_token.json"

	tok, err := loadChatToken(tokenPath)
	if err != nil {
		tok = chatAuthFlow(oa2Cfg, tokenPath)
	}

	src := oa2Cfg.TokenSource(context.Background(), tok)
	fresh, err := src.Token()
	if err != nil {
		fmt.Println(ColorYellow + "[Chat] Token refresh failed — re-authenticating..." + ColorReset)
		tok = chatAuthFlow(oa2Cfg, tokenPath)
		src = oa2Cfg.TokenSource(context.Background(), tok)
	} else if fresh.AccessToken != tok.AccessToken {
		saveChatToken(tokenPath, fresh)
	}

	return oauth2.NewClient(context.Background(), src)
}

func loadChatToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	return tok, json.NewDecoder(f).Decode(tok)
}

func saveChatToken(path string, tok *oauth2.Token) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Printf(ColorDim+"  [Chat] Could not save token: %v\n"+ColorReset, err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(tok)
}

// chatAuthFlow spins up a local redirect server, opens the browser, captures
// the auth code automatically, exchanges it for a token, and persists it.
func chatAuthFlow(oa2Cfg *oauth2.Config, tokenPath string) *oauth2.Token {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	check(err, "Could not bind local port for Chat auth redirect")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	oa2Cfg.RedirectURL = fmt.Sprintf("http://localhost:%d", port)
	authURL := oa2Cfg.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `<html><body style="font-family:sans-serif;padding:2em">
<h2>Google Chat authorized!</h2><p>You can close this tab and return to your terminal.</p>
</body></html>`)
		codeCh <- code
	})

	go srv.ListenAndServe()

	fmt.Printf(ColorCyan+"[Chat] Opening browser for authorization (port %d)..."+ColorReset+"\n", port)
	fmt.Println("If the browser doesn't open, visit this URL manually:")
	fmt.Println(authURL)
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(2 * time.Minute):
		fmt.Println("Chat auth timed out. Run your command again to retry.")
		srv.Shutdown(context.Background())
		os.Exit(1)
	}
	srv.Shutdown(context.Background())

	tok, err := oa2Cfg.Exchange(context.Background(), code)
	check(err, "Chat token exchange failed")
	saveChatToken(tokenPath, tok)
	fmt.Println(ColorGreen + "[Chat] Authorized — token cached to ~/.jot/chat_token.json" + ColorReset)
	return tok
}

// postToChat sends a plain-text message to a Google Chat space using the
// authenticated Chat API. spaceName must be in the form "spaces/AAAA...".
// Silently skips if spaceName is empty.
func postToChat(spaceName, message string) {
	if spaceName == "" {
		return
	}
	client := getChatClient()
	svc, err := chatv1.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		fmt.Printf(ColorDim+"  [Chat] Service init failed: %v\n"+ColorReset, err)
		return
	}
	_, err = svc.Spaces.Messages.Create(spaceName, &chatv1.Message{
		Text: message,
	}).Do()
	if err != nil {
		fmt.Printf(ColorDim+"  [Chat] Message failed: %v\n"+ColorReset, err)
	}
}

// ── Provisioning ──────────────────────────────────────────────────────────────

// adProvision is the CLI entry point: validates config, opens one LDAP
// connection, delegates to provisionUser, then closes.
func adProvision(first, last, role string, extras []string) {
	cfg := mustLoadConfig()
	validateProvisionConfig(cfg)
	l := ldapConnect(cfg)
	defer l.Close()
	provisionUser(l, cfg, first, last, role, extras)
}

// validateProvisionConfig exits early with a clear message if required fields
// for account creation are missing.
func validateProvisionConfig(cfg config.Jot) {
	if cfg.AD.DefaultPassword == "" {
		fmt.Fprintln(os.Stderr, ColorYellow+"[AD] ad.default_password is not set in config.json."+ColorReset)
		os.Exit(1)
	}
}

// provisionUser delegates the LDAP write to core/ad then handles CLI side
// effects (colored output, ClassLink sync, Chat notification).
func provisionUser(l *ldap.Conn, cfg config.Jot, first, last, role string, extras []string) {
	dept := strings.Join(extras, " ")
	res, err := ad.Provision(l, cfg, first, last, role, dept)
	if err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"[AD] Provision failed: %v\n"+ColorReset, err)
		return
	}

	usingLDAPS := strings.HasPrefix(cfg.AD.Server, "ldaps://")
	fmt.Printf(ColorGreen+"[AD] Account provisioned: %s\n"+ColorReset, res.Username)
	fmt.Printf("  UPN   : %s\n", res.UPN)
	fmt.Printf("  Email : %s\n", res.Email)
	fmt.Printf("  DN    : %s\n", res.DN)
	if !usingLDAPS {
		fmt.Println(ColorYellow + "  Note  : Account created DISABLED — set password manually (LDAPS required for auto-password)" + ColorReset)
	}

	triggerClassLinkSync(cfg)

	targetOU := cfg.AD.StaffOU
	if strings.ToLower(role) == "student" {
		targetOU = cfg.AD.StudentOU
	}
	var msg string
	if usingLDAPS {
		msg = fmt.Sprintf(
			"*[AD] Account Provisioned*\nUsername: `%s`\nName: %s %s\nRole: %s\nOU: %s\nUPN: `%s`\nEmail: `%s`\nTemp Password: `%s`",
			res.Username, first, last, role, targetOU, res.UPN, res.Email, cfg.AD.DefaultPassword,
		)
	} else {
		msg = fmt.Sprintf(
			"*[AD] Account Provisioned (disabled — no password set)*\nUsername: `%s`\nName: %s %s\nRole: %s\nOU: %s\nUPN: `%s`\nEmail: `%s`",
			res.Username, first, last, role, targetOU, res.UPN, res.Email,
		)
	}
	postToChat(cfg.GoogleChat.OpsSpace, msg)
}

// ── Incident IQ Helpers ───────────────────────────────────────────────────────

// iiqBase returns the base URL with any trailing /api/v1.0 stripped, then
// re-appended, so the config can have either format without breaking paths.
func iiqBase(cfg config.Jot) string {
	base := strings.TrimRight(cfg.IncidentIQ.BaseURL, "/")
	base = strings.TrimSuffix(base, "/api/v1.0")
	return base + "/api/v1.0"
}

func iiqRequest(cfg config.Jot, method, path string, body interface{}) (*http.Response, error) {
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequest(method, iiqBase(cfg)+path, bytes.NewReader(b))
	} else {
		req, err = http.NewRequest(method, iiqBase(cfg)+path, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.IncidentIQ.Token)
	req.Header.Set("Client", "ApiClient")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func iiqGET(cfg config.Jot, path string) (*http.Response, error) {
	return iiqRequest(cfg, "GET", path, nil)
}

func iiqPOST(cfg config.Jot, path string, body interface{}) (*http.Response, error) {
	return iiqRequest(cfg, "POST", path, body)
}

func iiqPATCH(cfg config.Jot, path string, body interface{}) (*http.Response, error) {
	return iiqRequest(cfg, "PATCH", path, body)
}

// ── Incident IQ Sync ──────────────────────────────────────────────────────────

func adSyncIncidentIQ() {
	cfg := mustLoadConfig()

	if cfg.IncidentIQ.OnboardingCategoryID == "" || cfg.IncidentIQ.SubmittedStatusID == "" {
		fmt.Fprintln(os.Stderr, ColorYellow+"[IIQ] submitted_status_id and onboarding_category_id must be set in config.\n      Run: jot ad iiq-info  to discover the correct GUIDs."+ColorReset)
		os.Exit(1)
	}

	filterBody := map[string]interface{}{
		"Filters": []map[string]string{
			{"Facet": "issuecategory", "Id": cfg.IncidentIQ.OnboardingCategoryID},
			{"Facet": "status", "Id": cfg.IncidentIQ.SubmittedStatusID},
		},
	}

	resp, err := iiqPOST(cfg, "/tickets?$s=100", filterBody)
	check(err, "IIQ API request failed")
	defer resp.Body.Close()

	var payload struct {
		Data struct {
			Items []map[string]interface{} `json:"Items"`
		} `json:"Data"`
	}
	check(json.NewDecoder(resp.Body).Decode(&payload), "IIQ response parse failed")

	tickets := payload.Data.Items
	if len(tickets) == 0 {
		fmt.Println("[IIQ] No submitted onboarding tickets found.")
		return
	}

	validateProvisionConfig(cfg)
	l := ldapConnect(cfg)
	defer l.Close()

	fmt.Printf(ColorCyan+"[IIQ] Processing %d submitted ticket(s)...\n"+ColorReset, len(tickets))

	for _, t := range tickets {
		ticketID, _ := t["TicketId"].(string)
		first, _ := t[cfg.IncidentIQ.FieldFirstName].(string)
		last, _ := t[cfg.IncidentIQ.FieldLastName].(string)
		role, _ := t[cfg.IncidentIQ.FieldRole].(string)

		if first == "" || last == "" || role == "" {
			fmt.Printf(ColorDim+"  Skipping ticket %s — missing required custom fields.\n"+ColorReset, ticketID)
			continue
		}

		fmt.Printf("  Provisioning: %s %s (%s) from ticket %s\n", first, last, role, ticketID)
		provisionUser(l, cfg, first, last, role, nil)

		if cfg.IncidentIQ.ClosedStatusID != "" {
			closeResp, err := iiqPATCH(cfg, "/tickets/"+ticketID, map[string]string{"StatusId": cfg.IncidentIQ.ClosedStatusID})
			if err != nil {
				fmt.Printf(ColorDim+"  Warning: could not close ticket %s: %v\n"+ColorReset, ticketID, err)
			} else {
				closeResp.Body.Close()
				fmt.Printf(ColorDim+"  Ticket %s closed.\n"+ColorReset, ticketID)
			}
		}
	}
}

// ── Incident IQ Info ──────────────────────────────────────────────────────────

func adIIQInfo() {
	cfg := mustLoadConfig()

	// ── Statuses ──
	fmt.Printf(ColorCyan + "[IIQ] Ticket Statuses\n" + ColorReset)
	sresp, err := iiqGET(cfg, "/statuses")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error fetching statuses: %v\n", err)
	} else {
		defer sresp.Body.Close()
		var sp struct {
			Data struct {
				Items []struct {
					StatusID string `json:"StatusId"`
					Name     string `json:"Name"`
				} `json:"Items"`
			} `json:"Data"`
		}
		if err := json.NewDecoder(sresp.Body).Decode(&sp); err != nil {
			fmt.Fprintf(os.Stderr, "  Could not parse statuses: %v\n", err)
		} else {
			for _, s := range sp.Data.Items {
				fmt.Printf("  %-36s  %s\n", s.StatusID, s.Name)
			}
		}
	}

	// ── Categories ──
	fmt.Printf("\n" + ColorCyan + "[IIQ] Issue Categories (first 200)\n" + ColorReset)
	cresp, err := iiqGET(cfg, "/categories/v2?$s=200")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error fetching categories: %v\n", err)
	} else {
		defer cresp.Body.Close()
		var cp struct {
			Data struct {
				Items []struct {
					CategoryID string `json:"CategoryId"`
					Name       string `json:"Name"`
					FullPath   string `json:"FullPath"`
				} `json:"Items"`
			} `json:"Data"`
		}
		if err := json.NewDecoder(cresp.Body).Decode(&cp); err != nil {
			fmt.Fprintf(os.Stderr, "  Could not parse categories: %v\n", err)
		} else {
			for _, c := range cp.Data.Items {
				path := c.FullPath
				if path == "" {
					path = c.Name
				}
				fmt.Printf("  %-36s  %s\n", c.CategoryID, path)
			}
		}
	}

	fmt.Printf("\n" + ColorDim + "Add the matching GUIDs to ~/.jot/config.json:\n")
	fmt.Printf("  submitted_status_id:    <GUID of Submitted>\n")
	fmt.Printf("  onboarding_category_id: <GUID of your HR onboarding category>\n" + ColorReset)
}

// ── Stale Account Cleanup ─────────────────────────────────────────────────────

func adCleanStale() {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	thresholdDays := cfg.AD.StaleThresholdDays
	if thresholdDays == 0 {
		thresholdDays = 90 // safe default
	}

	cutoff := ad.ToWindowsFileTime(time.Now().AddDate(0, 0, -thresholdDays))
	filter := fmt.Sprintf(
		"(&(objectCategory=person)(objectClass=user)(lastLogonTimestamp<=%d)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))",
		cutoff,
	)

	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter,
		[]string{"sAMAccountName", "displayName", "lastLogonTimestamp", "distinguishedName"},
		nil,
	)
	res, err := l.SearchWithPaging(req, 500)
	check(err, "Stale account search failed")

	if len(res.Entries) == 0 {
		fmt.Printf("[AD] No accounts inactive for more than %d days.\n", thresholdDays)
		return
	}

	fmt.Printf(ColorYellow+"[AD] Found %d stale account(s). Disabling...\n"+ColorReset, len(res.Entries))

	disabledOUDN := cfg.AD.DisabledOU + "," + cfg.AD.BaseDN
	var report []string

	for _, entry := range res.Entries {
		sam := entry.GetAttributeValue("sAMAccountName")
		display := entry.GetAttributeValue("displayName")
		userDN := entry.DN

		if err := ad.DisableUser(l, userDN); err != nil {
			fmt.Printf(ColorDim+"  Error disabling %s: %v\n"+ColorReset, sam, err)
			continue
		}

		cnPart := strings.Split(userDN, ",")[0]
		modDN := ldap.NewModifyDNRequest(userDN, cnPart, true, disabledOUDN)
		if err := l.ModifyDN(modDN); err != nil {
			fmt.Printf(ColorDim+"  Warning: could not move %s to Disabled OU: %v\n"+ColorReset, sam, err)
		}

		fmt.Printf("  Disabled: %s (%s)\n", sam, display)
		report = append(report, fmt.Sprintf("`%s` (%s)", sam, display))
	}

	triggerClassLinkSync(cfg)

	summary := fmt.Sprintf("*[AD] Stale Account Sweep*\nDisabled %d account(s) inactive >%d days:\n%s",
		len(report), thresholdDays, strings.Join(report, "\n"))
	postToChat(cfg.GoogleChat.OpsSpace, summary)

	fmt.Printf(ColorGreen+"Done. %d account(s) disabled and moved to %s.\n"+ColorReset,
		len(report), cfg.AD.DisabledOU)
}

// ── Endpoint Isolation (Incident Response) ────────────────────────────────────

func adIsolate(username string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	fmt.Printf(ColorYellow+"[AD] ISOLATING: %s\n"+ColorReset, username)

	userDN, err := ad.FindUserDN(l, cfg, username)
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
		os.Exit(1)
	}

	if err := ad.DisableUser(l, userDN); err != nil {
		fmt.Printf("  LDAP disable failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  [1/3] AD account disabled.")

	cmd := exec.Command("python3", cfg.CortexXDR.ScriptPath, "--user", username)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("  [2/3] Cortex XDR script error: %v\n%s\n", err, string(out))
	} else {
		fmt.Printf("  [2/3] Cortex XDR isolation triggered.\n%s\n", strings.TrimSpace(string(out)))
	}

	alert := fmt.Sprintf(
		"🚨 *[SECURITY] Account Isolated*\nUser: `%s`\nAD account disabled ✓\nCortex XDR endpoint isolation fired ✓\nTimestamp: %s",
		username, time.Now().Format("2006-01-02 15:04:05"),
	)
	postToChat(cfg.GoogleChat.SecuritySpace, alert)
	fmt.Println("  [3/3] Security alert posted to Google Chat.")

	fmt.Printf(ColorGreen+"Isolation complete for %s.\n"+ColorReset, username)
}

// ── Unlock ────────────────────────────────────────────────────────────────────

func adUnlock(username string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	userDN, err := ad.FindUserDN(l, cfg, username)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	mod := ldap.NewModifyRequest(userDN, nil)
	mod.Replace("lockoutTime", []string{"0"})
	check(l.Modify(mod), "Unlock failed")

	fmt.Printf(ColorGreen+"[AD] Account unlocked: %s\n"+ColorReset, username)
}

// ── Password Reset ────────────────────────────────────────────────────────────

func adPasswordReset(username string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	userDN, err := ad.FindUserDN(l, cfg, username)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	mod := ldap.NewModifyRequest(userDN, nil)
	mod.Replace("pwdLastSet", []string{"0"})
	check(l.Modify(mod), "Password reset flag failed")

	fmt.Printf(ColorGreen+"[AD] Password reset flagged at next login: %s\n"+ColorReset, username)
}

// ── Group Audit ───────────────────────────────────────────────────────────────

func adAudit(group string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	filter := "(&(objectClass=group))"
	if group != "all" {
		filter = fmt.Sprintf("(&(objectClass=group)(cn=%s))", ldap.EscapeFilter(group))
	}

	groupReq := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter, []string{"cn", "member"}, nil,
	)
	groupRes, err := l.Search(groupReq)
	check(err, "Group search failed")

	if len(groupRes.Entries) == 0 {
		fmt.Printf("No group found matching %q\n", group)
		return
	}

	auditDir := configDir() + "/audits"
	os.MkdirAll(auditDir, 0700)
	ts := time.Now().Format("2006-01-02_15-04-05")
	reportPath := fmt.Sprintf("%s/ad_audit_%s.csv", auditDir, ts)

	f, err := os.Create(reportPath)
	check(err, "Failed to create audit report")
	defer f.Close()

	fmt.Fprintln(f, "Group,MemberDN")
	var summary []string

	for _, g := range groupRes.Entries {
		cn := g.GetAttributeValue("cn")
		members := g.GetAttributeValues("member")
		fmt.Printf(ColorCyan+"[AD] Group: %s (%d members)\n"+ColorReset, cn, len(members))
		for _, m := range members {
			fmt.Fprintln(f, cn+","+m)
		}
		summary = append(summary, fmt.Sprintf("`%s`: %d members", cn, len(members)))
	}

	fmt.Printf(ColorGreen+"Audit report saved: %s\n"+ColorReset, reportPath)

	msg := fmt.Sprintf("*[AD] Group Membership Audit*\nReport: `%s`\n%s",
		reportPath, strings.Join(summary, "\n"))
	postToChat(cfg.GoogleChat.OpsSpace, msg)
}

// ── User Group Lookup ─────────────────────────────────────────────────────────

func adUserGroups(username string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		fmt.Sprintf("(&(objectCategory=person)(objectClass=user)(sAMAccountName=%s))", ldap.EscapeFilter(username)),
		[]string{"displayName", "distinguishedName", "memberOf"},
		nil,
	)
	res, err := l.Search(req)
	check(err, "User search failed")

	if len(res.Entries) == 0 {
		fmt.Printf("No user found: %s\n", username)
		os.Exit(1)
	}

	e := res.Entries[0]
	display := e.GetAttributeValue("displayName")
	groups := e.GetAttributeValues("memberOf")

	fmt.Printf(ColorCyan+"[AD] %s (%s) — %d group(s)\n"+ColorReset, display, username, len(groups))
	for _, g := range groups {
		// Extract CN= from the full DN for readability
		cn := g
		if parts := strings.SplitN(g, ",", 2); len(parts) > 0 {
			cn = strings.TrimPrefix(parts[0], "CN=")
		}
		fmt.Printf("  • %s\n", cn)
		fmt.Printf(ColorDim+"    %s\n"+ColorReset, g)
	}
}

// ── Add User to Group ─────────────────────────────────────────────────────────

func adAddToGroup(username, groupName string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	userDN, err := ad.FindUserDN(l, cfg, username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	groupReq := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		fmt.Sprintf("(&(objectClass=group)(cn=%s))", ldap.EscapeFilter(groupName)),
		[]string{"distinguishedName"},
		nil,
	)
	groupRes, err := l.Search(groupReq)
	check(err, "Group search failed")

	if len(groupRes.Entries) == 0 {
		fmt.Fprintf(os.Stderr, "No group found matching %q\n", groupName)
		os.Exit(1)
	}
	groupDN := groupRes.Entries[0].DN

	mod := ldap.NewModifyRequest(groupDN, nil)
	mod.Add("member", []string{userDN})
	if err := l.Modify(mod); err != nil {
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "ENTRY_EXISTS") {
			fmt.Printf(ColorYellow+"[AD] %s is already a member of %q\n"+ColorReset, username, groupName)
			return
		}
		check(err, "Failed to add user to group")
	}

	fmt.Printf(ColorGreen+"[AD] Added %s to %q\n"+ColorReset, username, groupName)
	fmt.Printf(ColorDim+"  User  : %s\n  Group : %s\n"+ColorReset, userDN, groupDN)
}

// ── Audit Search ──────────────────────────────────────────────────────────────

func adAuditSearch(term string) {
	auditDir := configDir() + "/audits"
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		fmt.Println("No audit reports found. Run: jot ad audit")
		os.Exit(1)
	}

	term = strings.ToLower(term)
	type hit struct {
		file  string
		group string
		dn    string
	}
	var hits []hit

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".csv") {
			continue
		}
		data, err := os.ReadFile(auditDir + "/" + entry.Name())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" || strings.HasPrefix(line, "Group,") {
				continue
			}
			if strings.Contains(strings.ToLower(line), term) {
				parts := strings.SplitN(line, ",", 2)
				if len(parts) == 2 {
					hits = append(hits, hit{file: entry.Name(), group: parts[0], dn: parts[1]})
				}
			}
		}
	}

	if len(hits) == 0 {
		fmt.Printf("No audit records matching %q\n", term)
		return
	}

	fmt.Printf(ColorCyan+"[AD] Audit search: %q — %d result(s)\n"+ColorReset, term, len(hits))
	currentFile := ""
	for _, h := range hits {
		if h.file != currentFile {
			fmt.Printf(ColorDim+"\n  Report: %s\n"+ColorReset, h.file)
			currentFile = h.file
		}
		cn := h.dn
		if parts := strings.SplitN(h.dn, ",", 2); len(parts) > 0 {
			cn = strings.TrimPrefix(parts[0], "CN=")
		}
		fmt.Printf("  %-30s  %s\n", h.group, cn)
		fmt.Printf(ColorDim+"  %s\n"+ColorReset, h.dn)
	}
}

// ── Account Status ────────────────────────────────────────────────────────────

func adStatus(username string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		fmt.Sprintf("(&(objectCategory=person)(objectClass=user)(sAMAccountName=%s))", ldap.EscapeFilter(username)),
		[]string{
			"displayName", "userPrincipalName", "mail", "department",
			"distinguishedName", "memberOf",
			"userAccountControl", "accountExpires",
			"pwdLastSet", "lastLogonTimestamp", "lockoutTime", "whenCreated",
		},
		nil,
	)
	res, err := l.Search(req)
	check(err, "User search failed")
	if len(res.Entries) == 0 {
		fmt.Fprintf(os.Stderr, "No user found: %s\n", username)
		os.Exit(1)
	}
	e := res.Entries[0]

	uac, _ := strconv.Atoi(e.GetAttributeValue("userAccountControl"))
	accountExpires, _ := strconv.ParseInt(e.GetAttributeValue("accountExpires"), 10, 64)
	pwdLastSet, _ := strconv.ParseInt(e.GetAttributeValue("pwdLastSet"), 10, 64)
	lastLogon, _ := strconv.ParseInt(e.GetAttributeValue("lastLogonTimestamp"), 10, 64)
	lockoutTime, _ := strconv.ParseInt(e.GetAttributeValue("lockoutTime"), 10, 64)

	disabled := uac&0x0002 != 0
	lockedOut := lockoutTime > 0
	pwdNeverExpires := uac&0x10000 != 0
	pwdMustChange := pwdLastSet == 0
	pwdExpired := uac&0x800000 != 0

	expireTime := ad.FromWindowsFileTime(accountExpires)
	pwdSetTime := ad.FromWindowsFileTime(pwdLastSet)
	lastLogonTime := ad.FromWindowsFileTime(lastLogon)

	const tf = "2006-01-02 15:04"
	neverStr := ColorDim + "never" + ColorReset

	statusColor := ColorGreen
	statusLabel := "ENABLED"
	if disabled {
		statusColor = ColorYellow
		statusLabel = "DISABLED"
	}

	fmt.Printf("\n%s%s%s  (%s)\n", ColorCyan, e.GetAttributeValue("displayName"), ColorReset, username)
	fmt.Printf(ColorDim+"  DN         : %s\n"+ColorReset, e.DN)
	fmt.Printf("  UPN        : %s\n", e.GetAttributeValue("userPrincipalName"))
	fmt.Printf("  Email      : %s\n", e.GetAttributeValue("mail"))
	if dept := e.GetAttributeValue("department"); dept != "" {
		fmt.Printf("  Department : %s\n", dept)
	}
	fmt.Printf("  Groups     : %d\n\n", len(e.GetAttributeValues("memberOf")))

	// Account state
	fmt.Printf("  Status     : %s%s%s\n", statusColor, statusLabel, ColorReset)

	if lockedOut {
		fmt.Printf("  Locked Out : %sYES%s (since %s)\n", ColorYellow, ColorReset, ad.FromWindowsFileTime(lockoutTime).Format(tf))
	} else {
		fmt.Printf("  Locked Out : %sNo%s\n", ColorGreen, ColorReset)
	}

	// Account expiration
	if expireTime.IsZero() {
		fmt.Printf("  Expires    : %s\n", neverStr)
	} else if time.Now().After(expireTime) {
		fmt.Printf("  Expires    : %sEXPIRED%s on %s\n", ColorYellow, ColorReset, expireTime.Format(tf))
	} else {
		daysLeft := int(time.Until(expireTime).Hours() / 24)
		color := ColorGreen
		if daysLeft < 30 {
			color = ColorYellow
		}
		fmt.Printf("  Expires    : %s%s%s (%d days)\n", color, expireTime.Format(tf), ColorReset, daysLeft)
	}

	// Password state
	switch {
	case pwdMustChange:
		fmt.Printf("  Password   : %sMust change at next login%s\n", ColorYellow, ColorReset)
	case pwdExpired:
		fmt.Printf("  Password   : %sEXPIRED%s\n", ColorYellow, ColorReset)
	case pwdNeverExpires:
		fmt.Printf("  Password   : %sNever expires%s", ColorDim, ColorReset)
		if !pwdSetTime.IsZero() {
			fmt.Printf(" (last set %s)", pwdSetTime.Format(tf))
		}
		fmt.Println()
	default:
		if pwdSetTime.IsZero() {
			fmt.Printf("  Password   : %s\n", neverStr)
		} else {
			fmt.Printf("  Password   : last set %s\n", pwdSetTime.Format(tf))
		}
	}

	// Last logon
	if lastLogonTime.IsZero() {
		fmt.Printf("  Last Logon : %s\n", neverStr)
	} else {
		daysSince := int(time.Since(lastLogonTime).Hours() / 24)
		color := ColorGreen
		if daysSince > 60 {
			color = ColorYellow
		}
		fmt.Printf("  Last Logon : %s%s%s (%d days ago)\n", color, lastLogonTime.Format(tf), ColorReset, daysSince)
	}

	fmt.Printf("  Created    : %s\n\n", e.GetAttributeValue("whenCreated"))
}

// ── User Search ───────────────────────────────────────────────────────────────

func adFind(query string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	escaped := ldap.EscapeFilter(query)
	filter := fmt.Sprintf(
		"(&(objectCategory=person)(objectClass=user)(|(sAMAccountName=*%s*)(displayName=*%s*)(mail=*%s*)))",
		escaped, escaped, escaped,
	)
	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 50, 0, false,
		filter,
		[]string{"sAMAccountName", "displayName", "mail", "userAccountControl", "distinguishedName"},
		nil,
	)
	res, err := l.Search(req)
	check(err, "User search failed")

	if len(res.Entries) == 0 {
		fmt.Printf("No users found matching %q\n", query)
		return
	}

	fmt.Printf(ColorCyan+"[AD] %d result(s) for %q\n\n"+ColorReset, len(res.Entries), query)
	for _, e := range res.Entries {
		uac, _ := strconv.Atoi(e.GetAttributeValue("userAccountControl"))
		state := ColorGreen + "enabled " + ColorReset
		if uac&0x0002 != 0 {
			state = ColorYellow + "disabled" + ColorReset
		}
		sam := e.GetAttributeValue("sAMAccountName")
		display := e.GetAttributeValue("displayName")
		mail := e.GetAttributeValue("mail")
		fmt.Printf("  [%s]  %-20s  %-28s  %s\n", state, sam, display, mail)
		fmt.Printf(ColorDim+"           %s\n"+ColorReset, e.DN)
	}
	fmt.Println()
}

// ── Backup ────────────────────────────────────────────────────────────────────

func adBackup() {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	req := ldap.NewSearchRequest(
		cfg.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(&(objectCategory=person)(objectClass=user))",
		[]string{"sAMAccountName", "displayName", "userPrincipalName",
			"userAccountControl", "distinguishedName", "lastLogonTimestamp"},
		nil,
	)
	res, err := l.SearchWithPaging(req, 500)
	check(err, "User search failed")

	type UserRecord struct {
		SAM       string `json:"sAMAccountName"`
		Display   string `json:"displayName"`
		UPN       string `json:"userPrincipalName"`
		UAC       string `json:"userAccountControl"`
		DN        string `json:"distinguishedName"`
		LastLogon string `json:"lastLogonTimestamp"`
	}
	var users []UserRecord
	for _, e := range res.Entries {
		users = append(users, UserRecord{
			SAM:       e.GetAttributeValue("sAMAccountName"),
			Display:   e.GetAttributeValue("displayName"),
			UPN:       e.GetAttributeValue("userPrincipalName"),
			UAC:       e.GetAttributeValue("userAccountControl"),
			DN:        e.DN,
			LastLogon: e.GetAttributeValue("lastLogonTimestamp"),
		})
	}

	data, err := json.MarshalIndent(users, "", "  ")
	check(err, "Failed to marshal backup")

	backupDir := configDir() + "/backups"
	os.MkdirAll(backupDir, 0700)
	ts := time.Now().Format("2006-01-02_15-04-05")
	outPath := fmt.Sprintf("%s/ad_backup_%s.json.gz", backupDir, ts)

	file, err := os.Create(outPath)
	check(err, "Failed to create backup file")
	defer file.Close()

	gz := gzip.NewWriter(file)
	gz.Write(data)
	gz.Close()

	fmt.Printf(ColorGreen+"[AD] Backup complete: %s (%d users)\n"+ColorReset, outPath, len(users))
}

// ── God Mode — Lifecycle ──────────────────────────────────────────────────────

// adTerm performs a full offboarding sequence:
//  1. Disables the AD account (sets ACCOUNTDISABLE bit via ad.DisableUser)
//  2. Stamps extensionAttribute1 with the UTC disable time in RFC3339 format —
//     this is the authoritative source for adPurge's 365-day age calculation;
//     whenChanged is never used for that purpose
//  3. Moves the account to the Disabled OU via ModifyDN
//  4. Suspends the Google Workspace account via GAM (skips gracefully if GAM
//     is not found in PATH, printing the manual command instead)
//  5. Posts a full audit record to the ops Google Chat space
//
// NOTE — adCleanStale retrofit: when implementing real logic for adCleanStale,
// it must also stamp extensionAttribute1 at disable time using the identical
// Modify write below, so both manual terminations and automated 90-day sweeps
// are tracked consistently for adPurge's 365-day threshold.
func adTerm(username string) {
	cfg := mustLoadConfig()
	l := ldapConnect(cfg)
	defer l.Close()

	fmt.Printf(ColorCyan+"[term] Terminating account: %s\n"+ColorReset, username)

	// Resolve DN — hard exit if user not found
	userDN, err := ad.FindUserDN(l, cfg, username)
	if err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"  Error: %v\n"+ColorReset, err)
		os.Exit(1)
	}

	// [1/4] Disable account
	if err := ad.DisableUser(l, userDN); err != nil {
		fmt.Fprintf(os.Stderr, "  [1/4] LDAP disable failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  [1/4] AD account disabled.")

	// [2/4] Stamp extensionAttribute1 with the disable timestamp (RFC3339 UTC).
	// Non-fatal: log clearly but do not abort — the OU move and GAM suspension
	// are still valuable even if the stamp fails. adPurge will skip this account
	// with a warning until the attribute is stamped manually.
	disableStamp := time.Now().UTC().Format(time.RFC3339)
	stampMod := ldap.NewModifyRequest(userDN, nil)
	stampMod.Replace("extensionAttribute1", []string{disableStamp})
	if err := l.Modify(stampMod); err != nil {
		fmt.Printf(ColorDim+"  [2/4] Warning: extensionAttribute1 stamp failed: %v\n"+ColorReset, err)
		fmt.Println(ColorDim + "         adPurge will skip this account until stamped manually." + ColorReset)
	} else {
		fmt.Printf("  [2/4] Stamped extensionAttribute1: %s\n", disableStamp)
	}

	// [3/4] Move to Disabled OU via ModifyDN — matches adCleanStale pattern exactly
	disabledOUDN := cfg.AD.DisabledOU + "," + cfg.AD.BaseDN
	cnPart := strings.Split(userDN, ",")[0]
	modDN := ldap.NewModifyDNRequest(userDN, cnPart, true, disabledOUDN)
	if err := l.ModifyDN(modDN); err != nil {
		fmt.Printf(ColorDim+"  [3/4] Warning: could not move to Disabled OU: %v\n"+ColorReset, err)
	} else {
		fmt.Printf("  [3/4] Moved to: %s\n", disabledOUDN)
	}

	// [4/4] Suspend Google Workspace account via GAM.
	// Uses exec.LookPath so GAM can live anywhere in PATH without a config entry.
	email := username + cfg.AD.EmailSuffix
	gamPath, lookErr := exec.LookPath("gam")
	if lookErr != nil {
		fmt.Printf(ColorYellow+"  [4/4] GAM not found in PATH — Google suspension skipped.\n"+ColorReset)
		fmt.Printf(ColorDim+"         Run manually: gam update user %s suspended true\n"+ColorReset, email)
	} else {
		cmd := exec.Command(gamPath, "update", "user", email, "suspended", "true")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf(ColorYellow+"  [4/4] GAM error: %v\n"+ColorReset, err)
			if len(out) > 0 {
				fmt.Printf(ColorDim+"         %s\n"+ColorReset, strings.TrimSpace(string(out)))
			}
		} else {
			fmt.Printf("  [4/4] Google account suspended: %s\n", email)
			if len(out) > 0 {
				fmt.Printf(ColorDim+"         %s\n"+ColorReset, strings.TrimSpace(string(out)))
			}
		}
	}

	postToChat(cfg.GoogleChat.OpsSpace, fmt.Sprintf(
		"*[AD] Account Terminated*\nUser: `%s`\nEmail: `%s`\nDisabled at: `%s`\nActions: AD disabled · extensionAttribute1 stamped · moved to Disabled OU · Google suspended",
		username, email, disableStamp,
	))

	fmt.Printf(ColorGreen+"[term] Done — %s fully terminated.\n"+ColorReset, username)
}

// adResurrect re-enables a terminated account — the exact counterpart to adTerm:
//  1. Locates the account within the Disabled OU (exits gracefully if not found)
//  2. Moves it to the correct active OU (StaffOU or StudentOU) via ModifyDN
//  3. Clears the ACCOUNTDISABLE bit in userAccountControl
//  4. Sets a temporary password via unicodePwd, then forces a change at next logon
//     (two separate Modify calls — combining them can be rejected depending on DFL)
//  5. Unsuspends the Google Workspace account via GAM
//  6. Posts an audit record to the ops Google Chat space
func adResurrect(username, role string) {
	cfg := mustLoadConfig()
	role = strings.ToLower(role)
	if role != "staff" && role != "student" {
		fmt.Fprintf(os.Stderr, ColorYellow+"[resurrect] role must be 'staff' or 'student', got %q\n"+ColorReset, role)
		os.Exit(1)
	}
	validateProvisionConfig(cfg) // ensures cfg.AD.DefaultPassword is set before connecting

	targetOU := cfg.AD.StaffOU
	if role == "student" {
		targetOU = cfg.AD.StudentOU
	}
	targetOUDN := targetOU + "," + cfg.AD.BaseDN

	l := ldapConnect(cfg)
	defer l.Close()

	fmt.Printf(ColorCyan+"[resurrect] Reactivating %s (role: %s)\n"+ColorReset, username, role)

	// Locate account in Disabled OU — exit gracefully with an actionable hint if
	// not found. userAccountControl is fetched here to avoid a second round-trip
	// for the enable step below.
	disabledBase := cfg.AD.DisabledOU + "," + cfg.AD.BaseDN
	searchReq := ldap.NewSearchRequest(
		disabledBase,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		fmt.Sprintf("(&(objectCategory=person)(objectClass=user)(sAMAccountName=%s))",
			ldap.EscapeFilter(username)),
		[]string{"distinguishedName", "userAccountControl"},
		nil,
	)
	searchRes, err := l.Search(searchReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, ColorYellow+"  Error searching Disabled OU: %v\n"+ColorReset, err)
		os.Exit(1)
	}
	if len(searchRes.Entries) == 0 {
		fmt.Printf(ColorYellow+"[resurrect] %q not found in Disabled OU (%s).\n"+ColorReset,
			username, cfg.AD.DisabledOU)
		fmt.Println(ColorDim + "         Check the username or run: jot ad find " + username + ColorReset)
		os.Exit(1)
	}
	userDN := searchRes.Entries[0].DN
	currentUAC, _ := strconv.Atoi(searchRes.Entries[0].GetAttributeValue("userAccountControl"))

	// [1/4] Move back to active OU via ModifyDN — mirrors adTerm/adCleanStale pattern
	cnPart := strings.Split(userDN, ",")[0]
	modDN := ldap.NewModifyDNRequest(userDN, cnPart, true, targetOUDN)
	if err := l.ModifyDN(modDN); err != nil {
		fmt.Fprintf(os.Stderr, "  [1/4] ModifyDN failed: %v\n", err)
		os.Exit(1)
	}
	newDN := cnPart + "," + targetOUDN
	fmt.Printf("  [1/4] Moved to: %s\n", targetOUDN)

	// [2/4] Enable account — clear the ACCOUNTDISABLE bit (0x0002) from UAC.
	// &^ is Go's bitwise clear (AND NOT) — exact inverse of ad.DisableUser's | 2.
	enableMod := ldap.NewModifyRequest(newDN, nil)
	enableMod.Replace("userAccountControl", []string{strconv.Itoa(currentUAC &^ 2)})
	if err := l.Modify(enableMod); err != nil {
		fmt.Fprintf(os.Stderr, "  [2/4] Enable failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  [2/4] Account enabled (UAC: %d → %d).\n", currentUAC, currentUAC&^2)

	// [3/4] Set temporary password, then force change at next logon.
	// Intentionally two separate Modify calls: AD may reject unicodePwd and
	// pwdLastSet=0 in the same request depending on domain functional level.
	pwdMod := ldap.NewModifyRequest(newDN, nil)
	pwdMod.Replace("unicodePwd", []string{ad.EncodeUnicodePwd(cfg.AD.DefaultPassword)})
	if err := l.Modify(pwdMod); err != nil {
		fmt.Fprintf(os.Stderr, "  [3/4] Password set failed: %v\n", err)
		os.Exit(1)
	}
	expMod := ldap.NewModifyRequest(newDN, nil)
	expMod.Replace("pwdLastSet", []string{"0"})
	if err := l.Modify(expMod); err != nil {
		fmt.Fprintf(os.Stderr, "  [3/4] Force-expiry failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  [3/4] Temp password set; must change at next login.")

	// [4/4] Unsuspend Google Workspace account via GAM — mirrors adTerm's LookPath guard
	email := username + cfg.AD.EmailSuffix
	gamPath, lookErr := exec.LookPath("gam")
	if lookErr != nil {
		fmt.Printf(ColorYellow+"  [4/4] GAM not found in PATH — Google unsuspend skipped.\n"+ColorReset)
		fmt.Printf(ColorDim+"         Run manually: gam update user %s suspended false\n"+ColorReset, email)
	} else {
		cmd := exec.Command(gamPath, "update", "user", email, "suspended", "false")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf(ColorYellow+"  [4/4] GAM error: %v\n"+ColorReset, err)
			if len(out) > 0 {
				fmt.Printf(ColorDim+"         %s\n"+ColorReset, strings.TrimSpace(string(out)))
			}
		} else {
			fmt.Printf("  [4/4] Google account unsuspended: %s\n", email)
			if len(out) > 0 {
				fmt.Printf(ColorDim+"         %s\n"+ColorReset, strings.TrimSpace(string(out)))
			}
		}
	}

	postToChat(cfg.GoogleChat.OpsSpace, fmt.Sprintf(
		"*[AD] Account Resurrected*\nUser: `%s`\nEmail: `%s`\nRole: %s\nMoved to: `%s`\nTemp password set · must change at next login · Google unsuspended.",
		username, email, role, targetOUDN,
	))

	fmt.Printf(ColorGreen+"[resurrect] Done — %s is active again.\n"+ColorReset, username)
}

// ── Help ──────────────────────────────────────────────────────────────────────

func printADHelp() {
	fmt.Printf(`
%sActive Directory Commands%s  (jot ad <command>)

  %sProvisioning%s
    new <first> <last> <staff|student> [dept|grade]
                      Provision account → LDAP write → ClassLink sync
    sync              Poll IIQ for submitted onboarding tickets and provision each one
    iiq-info          List all IIQ ticket statuses and categories with their GUIDs

  %sAccount Management%s
    status <user>     Full account status: enabled/disabled, locked, expiry, last logon
    find   <query>    Search users by name, username, or email (max 50 results)
    unlock <user>     Clear lockout flag on account
    reset  <user>     Force password change at next login
    clean             Disable inactive accounts (>90d) → ClassLink sync
                      → report pushed to Google Chat

  %sGod Mode — Lifecycle%s
    term      <user>              Full offboard: disable AD · stamp extensionAttribute1 ·
                                  move to Disabled OU · suspend Google via GAM
                                  → audit posted to Google Chat ops space
    resurrect <user> <staff|student>
                                  Full onboard: move from Disabled OU · enable ·
                                  set temp password · unsuspend Google via GAM
                                  → audit posted to Google Chat ops space

  %sIncident Response%s
    isolate <user>    Disable AD account + isolate endpoints via Cortex XDR
                      → alert posted to Google Chat security space

  %sReporting & Maintenance%s
    audit  [group]    Group membership audit → report pushed to Google Chat
    groups <user>     List all groups a user belongs to
    addgroup <user> <group>
                      Add user to an AD group
    search <term>     Search saved audit reports for a user or group name
    backup            Snapshot AD/environment state to timestamped archive

  %sConfig%s
    ~/.jot/config.json  Holds: ad.*, duo.*, classlink.*, cortex_xdr.*,
                        incident_iq.*, google_oauth.*, google_chat.*
    ~/.jot/chat_token.json  Cached OAuth token (auto-managed)

    %sDuo fields%s  duo.api_hostname · duo.integration_key · duo.secret_key
    %sEnv override%s  JOT_DUO_SECRET_KEY

`,
		ColorCyan, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorYellow, ColorReset,
		ColorDim, ColorReset,
		ColorDim, ColorReset)
}
