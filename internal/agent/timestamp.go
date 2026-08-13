package agent

import (
	"encoding/json"
	"strconv"
	"time"
)

func extractTimestamp(line string) int64 {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return 0
	}
	for _, key := range []string{"timestamp", "ts", "time"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		if len(v) == 0 {
			continue
		}

		if v[0] == '"' {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				continue
			}

			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t.UnixNano()
			}

			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return normalizeEpoch(n)
			}
			continue
		}

		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return normalizeEpoch(n)
		}
	}
	return 0
}

func normalizeEpoch(n int64) int64 {
	switch {
	case n < 1e11:
		return n * int64(time.Second)
	case n < 1e14:
		return n * int64(time.Millisecond)
	case n < 1e17:
		return n * int64(time.Microsecond)
	default:
		return n
	}
}
