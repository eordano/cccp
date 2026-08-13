package pravda

import (
	"strconv"
	"time"
)

var tsFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02T15:04:05Z",
}

func parseTS(s string) (time.Time, bool) {
	for _, layout := range tsFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Local(), true
		}
	}
	return time.Time{}, false
}

func formatStamp(s string) string {
	t, ok := parseTS(s)
	if !ok {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

func formatClock(s string) string {
	t, ok := parseTS(s)
	if !ok {
		return ""
	}
	return t.Format("15:04:05")
}

func formatDuration(d time.Duration) string {
	s := int(d.Seconds())
	m := s / 60
	s = s % 60
	if m > 0 {
		return itoa(m) + "m " + itoa(s) + "s"
	}
	return itoa(s) + "s"
}

func formatMs(ms int64) string {
	if ms < 1000 {
		return itoa(int(ms)) + "ms"
	}
	return formatDuration(time.Duration(ms) * time.Millisecond)
}

func itoa(n int) string { return strconv.Itoa(n) }

func formatSize(bytes int64) string {
	switch {
	case bytes <= 0:
		return "--"
	case bytes < 1024:
		return strconv.FormatInt(bytes, 10) + "B"
	case bytes < 1024*1024:
		return strconv.FormatFloat(float64(bytes)/1024.0, 'f', 1, 64) + "KB"
	default:
		return strconv.FormatFloat(float64(bytes)/1024.0/1024.0, 'f', 1, 64) + "MB"
	}
}
