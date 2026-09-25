package archive

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// timeLayout is the one stored timestamp format: fixed-width UTC with
// milliseconds, so string order equals time order in SQL comparisons.
const timeLayout = "2006-01-02T15:04:05.000Z"

func now() string { return formatTime(time.Now()) }

func formatTime(value time.Time) string { return value.UTC().Format(timeLayout) }

// canonicalTime converts any known timestamp shape to timeLayout, or returns ""
// when value is empty or cannot be parsed. Zone-less values are UTC because
// they come from SQLite CURRENT_TIMESTAMP/datetime('now') in source databases.
func canonicalTime(value any) string {
	switch item := value.(type) {
	case nil:
		return ""
	case time.Time:
		if item.IsZero() {
			return ""
		}
		return formatTime(item)
	case float64:
		return epochTime(item)
	case int64:
		return epochTime(float64(item))
	case int:
		return epochTime(float64(item))
	case json.Number:
		number, err := item.Float64()
		if err != nil {
			return ""
		}
		return epochTime(number)
	case string:
		text := strings.TrimSpace(item)
		if parsed, ok := parseTime(text); ok {
			return formatTime(parsed)
		}
		if epochText.MatchString(text) {
			return canonicalTime(json.Number(text))
		}
	}
	return ""
}

var epochText = regexp.MustCompile(`^[0-9]{9,13}(\.[0-9]+)?$`)

// epochTime accepts seconds or milliseconds since the Unix epoch, rounding to
// the nearest millisecond so float error cannot shift the stored value.
func epochTime(value float64) string {
	millis := value
	if value <= 10_000_000_000 {
		millis *= 1000
	}
	return formatTime(time.UnixMilli(int64(math.Round(millis))))
}

// timeBound converts a search bound for string comparison with stored
// timestamps. A date-only upper bound covers that whole day.
func timeBound(value string, end bool) string {
	parsed, ok := parseTime(strings.TrimSpace(value))
	if !ok {
		return value
	}
	if end && len(strings.TrimSpace(value)) == len("2006-01-02") {
		parsed = parsed.Add(24*time.Hour - time.Millisecond)
	}
	return formatTime(parsed)
}

func stableID(namespace string, parts ...any) string {
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case nil:
			values = append(values, "None")
		case bool:
			if value {
				values = append(values, "True")
			} else {
				values = append(values, "False")
			}
		default:
			values = append(values, fmt.Sprint(part))
		}
	}
	// Match Python's uuid.uuid5(uuid.NAMESPACE_URL, ...), preserving every
	// catalog identifier written by the original implementation.
	namespaceURL := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	hash := sha1.New()
	_, _ = hash.Write(namespaceURL)
	_, _ = hash.Write([]byte(namespace + ":" + strings.Join(values, "\x1f")))
	value := hash.Sum(nil)[:16]
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	return namespace + "_" + hex.EncodeToString(value)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func jsonText(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(asString(value)); text != "" && text != "null" {
			return text
		}
	}
	return ""
}

func expandPath(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, _ := os.UserHomeDir()
		if value == "~" {
			return home
		}
		return filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	return value
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	// Go accepts a fractional second after the seconds field even when the
	// layout omits it, so these cover every fraction width.
	formats := []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, format := range formats {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

var words = regexp.MustCompile(`[[:alnum:]_./:-]+`)

func ftsQuery(value string) string {
	parts := words.FindAllString(value, -1)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, `"`+strings.ReplaceAll(part, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " AND ")
}

func volumeIdentity(path string) string {
	resolved, err := filepath.Abs(path)
	if err != nil {
		resolved = path
	}
	for {
		output, err := exec.Command("diskutil", "info", "-plist", resolved).Output()
		if err == nil {
			text := string(output)
			for _, key := range []string{"VolumeUUID", "DiskUUID"} {
				marker := "<key>" + key + "</key>"
				if at := strings.Index(text, marker); at >= 0 {
					rest := text[at+len(marker):]
					start := strings.Index(rest, "<string>")
					end := strings.Index(rest, "</string>")
					if start >= 0 && end > start {
						return "uuid:" + rest[start+8:end]
					}
				}
			}
		}
		parent := filepath.Dir(resolved)
		if parent == resolved {
			break
		}
		resolved = parent
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err == nil {
		return fmt.Sprintf("device:%d", stat.Dev)
	}
	return "unavailable"
}

func diskUsage(path string) (total, free uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	return stat.Blocks * uint64(stat.Bsize), stat.Bavail * uint64(stat.Bsize)
}

func treeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, infoErr := entry.Info(); infoErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
