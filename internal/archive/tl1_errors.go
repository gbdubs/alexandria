package archive

import (
	"regexp"
	"strings"
)

// TL1 error attribution says who can fix a failure. Infrastructure failures
// (dead workers, restarts) say nothing about the agent configuration that was
// running, so configuration comparisons exclude them.
const (
	tl1AttributionInfrastructure = "infrastructure"
	tl1AttributionConfiguration  = "configuration"
	tl1AttributionContract       = "output_contract"
	tl1AttributionScript         = "script"
	tl1AttributionProvider       = "provider"
	tl1AttributionAgent          = "agent"
)

var (
	tl1WorkerDied      = regexp.MustCompile(`(?i)^worker \S+ died`)
	tl1ExecutorExit    = regexp.MustCompile(`(?i)^(codex|claude) exited with code (-?\d+)`)
	tl1ScriptExit      = regexp.MustCompile(`(?i)script exited with code (-?\d+)`)
	tl1SignatureHex    = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)
	tl1SignatureUUID   = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	tl1SignaturePath   = regexp.MustCompile(`(?:~|/)[^\s'"()\[\]{}:,]*/[^\s'"()\[\]{}:,]+`)
	tl1SignatureNumber = regexp.MustCompile(`\b\d{3,}\b`)
	tl1SignatureSpace  = regexp.MustCompile(`\s+`)
	tl1ReasonCode      = regexp.MustCompile(`\b[a-z]+(?:_[a-z]+)+\b`)
	tl1SignatureSpan   = regexp.MustCompile(`\b\d+(\.\d+)?\s*(ms|s|sec|secs|seconds?|minutes?|mins?|hours?|h|m)\b`)
)

// tl1ExecutorNoise are lines agent CLIs print before the real failure.
var tl1ExecutorNoise = []string{"reading additional input from stdin", "reading prompt from stdin"}

// classifyTL1Error groups a TL1 failure explanation into a stable class, a
// signature with run-specific detail removed (so identical failures cluster),
// and the attribution of who can fix it.
func classifyTL1Error(text string) (class, signature, attribution string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", ""
	}
	lower := strings.ToLower(text)
	first := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	switch {
	case tl1WorkerDied.MatchString(first):
		return "worker_died", "worker died", tl1AttributionInfrastructure
	case strings.Contains(lower, "execution interrupted by process restart"):
		return "process_restart", "execution interrupted by process restart", tl1AttributionInfrastructure
	case strings.Contains(lower, "stuck from previous run"):
		return "stuck_previous_run", "stuck from previous run", tl1AttributionInfrastructure
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit") || strings.Contains(lower, "429") || strings.Contains(lower, "overloaded") || strings.Contains(lower, "quota"):
		return "rate_limited", tl1Signature(first), tl1AttributionProvider
	case strings.HasPrefix(lower, "contract_repair_exhausted") || strings.Contains(lower, "invalid shape declaration") || strings.Contains(lower, "missing_output_fields") || strings.Contains(lower, "missing output field") || strings.Contains(lower, "invalid_output_type") || strings.Contains(lower, "marker_missing") || strings.Contains(lower, "handoff marker"):
		return "output_contract", tl1Signature(tl1ContractSignature(text)), tl1AttributionContract
	}
	if match := tl1ExecutorExit.FindStringSubmatch(first); match != nil {
		detail := ""
		for _, line := range strings.Split(strings.TrimSpace(text[len(match[0]):]), "\n") {
			line = strings.TrimSpace(strings.TrimLeft(line, ": "))
			if line == "" || tl1Noise(line) {
				continue
			}
			detail = line
			break
		}
		signature = strings.ToLower(match[1]) + " exited with code " + match[2]
		if detail != "" {
			signature += ": " + detail
		}
		return "executor_exit", tl1Signature(signature), tl1AttributionConfiguration
	}
	if match := tl1ScriptExit.FindStringSubmatch(text); match != nil {
		return "script_exit", "script exited with code " + match[1], tl1AttributionScript
	}
	if strings.Contains(lower, "timed out") || strings.Contains(lower, "timeout") {
		return "timeout", tl1Signature(first), tl1AttributionAgent
	}
	if lower == "runtime_error" {
		return "unexplained", "runtime_error without a recorded reason", tl1AttributionAgent
	}
	return "other", tl1Signature(first), tl1AttributionAgent
}

func tl1Noise(line string) bool {
	lower := strings.ToLower(line)
	for _, noise := range tl1ExecutorNoise {
		if strings.HasPrefix(lower, noise) {
			return true
		}
	}
	return false
}

// tl1ContractSignature keeps the snake_case reason codes of an output-contract
// failure ("contract_repair_exhausted: missing_output_fields: …; marker_missing")
// and drops the per-run detail between them.
func tl1ContractSignature(text string) string {
	first := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if location := tl1ReasonCode.FindStringIndex(first); location == nil || location[0] != 0 {
		return first
	}
	seen := map[string]bool{}
	codes := []string{}
	for _, code := range tl1ReasonCode.FindAllString(first, -1) {
		if !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return strings.Join(codes, ": ")
}

// tl1Signature removes identifiers, paths, and large numbers so the same
// failure in different runs has the same text.
func tl1Signature(text string) string {
	text = tl1SignatureUUID.ReplaceAllString(text, "<id>")
	text = tl1SignatureHex.ReplaceAllString(text, "<id>")
	text = tl1SignaturePath.ReplaceAllString(text, "<path>")
	text = tl1SignatureSpan.ReplaceAllString(text, "<n> $2")
	text = tl1SignatureNumber.ReplaceAllString(text, "<n>")
	text = strings.TrimSpace(tl1SignatureSpace.ReplaceAllString(text, " "))
	if len(text) > 180 {
		text = strings.ToValidUTF8(text[:180], "") + "…"
	}
	return text
}
