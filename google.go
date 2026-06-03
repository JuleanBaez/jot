package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	tasks "google.golang.org/api/tasks/v1"
)

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: home directory:", err)
		os.Exit(1)
	}
	return home + "/.jot"
}

func getTasksService() (*tasks.Service, error) {
	credPath := configDir() + "/credentials.json"
	b, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("credentials not found at %s", credPath)
	}

	cfg, err := google.ConfigFromJSON(b, tasks.TasksScope)
	if err != nil {
		return nil, fmt.Errorf("unable to parse credentials: %v", err)
	}

	tok, err := loadToken()
	if err != nil {
		return nil, err
	}

	client := cfg.Client(context.Background(), tok)
	return tasks.NewService(context.Background(), option.WithHTTPClient(client))
}

func loadToken() (*oauth2.Token, error) {
	f, err := os.Open(configDir() + "/token.json")
	if err != nil {
		return nil, fmt.Errorf("not authenticated — run 'jot auth' to connect Google Tasks")
	}
	defer f.Close()

	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, fmt.Errorf("invalid token — run 'jot auth' to re-authenticate")
	}
	return tok, nil
}

func syncTaskToGoogle(title string) {
	srv, err := getTasksService()
	if err != nil {
		fmt.Println(ColorDim + "  (Run 'jot auth' to enable Google Tasks sync)" + ColorReset)
		return
	}

	_, err = srv.Tasks.Insert("@default", &tasks.Task{Title: title}).Do()
	if err != nil {
		fmt.Printf("Warning: Google Tasks sync failed: %v\n", err)
		return
	}
	fmt.Println(ColorCyan + "  Synced to Google Tasks!" + ColorReset)
}

func setupGoogleAuth() {
	dir := configDir()
	credPath := dir + "/credentials.json"

	b, err := os.ReadFile(credPath)
	if err != nil {
		fmt.Printf(`%sSetup Required%s

To use Google Tasks integration:

  1. Go to https://console.cloud.google.com/
  2. Create a project and enable the Google Tasks API
  3. Go to APIs & Services -> Credentials
  4. Create OAuth 2.0 credentials (Application type: Desktop app)
  5. Download the credentials JSON file
  6. Save it to: %s

Then run: jot auth

`, ColorYellow, ColorReset, credPath)
		return
	}

	cfg, err := google.ConfigFromJSON(b, tasks.TasksScope)
	check(err, "Unable to parse credentials file")

	// Bind a random free port for the local redirect server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	check(err, "Unable to bind a local port for auth redirect")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	// Google supports loopback redirects on any port (RFC 8252)
	cfg.RedirectURL = fmt.Sprintf("http://localhost:%d", port)

	authURL := cfg.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Missing code parameter", http.StatusBadRequest)
			errCh <- fmt.Errorf("no code in redirect")
			return
		}
		fmt.Fprintf(w, `<html><body style="font-family:sans-serif;padding:2em">
<h2>Authorization successful!</h2>
<p>You can close this tab and return to your terminal.</p>
</body></html>`)
		codeCh <- code
	})

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf(ColorCyan+"Opening browser for Google authentication (listening on port %d)..."+ColorReset+"\n", port)
	fmt.Println("If the browser doesn't open, visit this URL manually:")
	fmt.Println(authURL)
	fmt.Println()
	openBrowser(authURL)

	// Wait for code or timeout after 2 minutes
	var code string
	select {
	case code = <-codeCh:
	case e := <-errCh:
		fmt.Printf("Auth error: %v\n", e)
		srv.Shutdown(context.Background())
		return
	case <-time.After(2 * time.Minute):
		fmt.Println("Authentication timed out. Run 'jot auth' to try again.")
		srv.Shutdown(context.Background())
		return
	}

	srv.Shutdown(context.Background())

	tok, err := cfg.Exchange(context.Background(), code)
	check(err, "Failed to exchange authorization code")

	os.MkdirAll(dir, 0700)
	f, err := os.OpenFile(dir+"/token.json", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	check(err, "Unable to save token")
	defer f.Close()

	json.NewEncoder(f).Encode(tok)
	fmt.Println(ColorGreen + "Google Tasks integration is now active! Tasks will sync automatically." + ColorReset)
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}
