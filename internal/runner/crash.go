package runner

import "strings"

// scanCrash looks for an app crash in a logcat dump. Returns a short description
// of the first match, or "" if the log looks clean.
func scanCrash(log, pkg string) string {
	if log == "" {
		return ""
	}
	lines := strings.Split(log, "\n")
	for i, ln := range lines {
		switch {
		case strings.Contains(ln, "FATAL EXCEPTION"):
			// the process line usually follows; include it if it names the app
			detail := strings.TrimSpace(ln)
			if i+1 < len(lines) && strings.Contains(lines[i+1], "Process:") {
				if pkg == "" || strings.Contains(lines[i+1], pkg) {
					return detail + " — " + strings.TrimSpace(lines[i+1])
				}
				continue // a different app crashed, not ours
			}
			return detail
		case pkg != "" && strings.Contains(ln, "ANR in "+pkg):
			return strings.TrimSpace(ln)
		case pkg != "" && strings.Contains(ln, "Force finishing activity") && strings.Contains(ln, pkg):
			return strings.TrimSpace(ln)
		}
	}
	return ""
}
