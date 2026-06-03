package dash

import (
	"fmt"
	"os/exec"
	"runtime"
)

// openURL launches the operator's default browser pointing at url. It never
// blocks on the browser — cmd.Start forks and returns immediately, which is
// what we want because the dash server is already running and we don't care
// about the child process's exit code.
//
// Failure here is not fatal: if the browser can't be launched (headless box,
// no DISPLAY, missing xdg-open, etc.) the server keeps serving and the
// operator can just visit the URL manually. Run() logs the error and
// continues.
func openURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// rundll32 is the most reliable cross-version Windows path — cmd.exe
		// builtin "start" requires a shell and has quoting quirks around
		// URLs with & in the querystring.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "linux", "freebsd", "netbsd", "openbsd":
		cmd = exec.Command("xdg-open", url)
	default:
		return fmt.Errorf("no browser launcher known for GOOS=%s", runtime.GOOS)
	}
	return cmd.Start()
}
