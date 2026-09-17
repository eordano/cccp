package pravda

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
)

type Converter func(line []byte) [][]byte

var converters = map[string]Converter{}

var sessionFilters = map[string]func(rel string) bool{}

func RegisterConverter(agent string, c Converter) {
	converters[agent] = c
}

func RegisterSessionFilter(agent string, keep func(rel string) bool) {
	sessionFilters[agent] = keep
}

func isListedSession(path string) bool {
	a := agentOf(path)
	if a == "" {
		return true
	}
	keep := sessionFilters[a]
	if keep == nil {
		return true
	}
	return keep(strings.TrimPrefix(path, "agent/"+a+"/"))
}

func agentOf(path string) string {
	rest, ok := strings.CutPrefix(path, "agent/")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i > 0 {
		return rest[:i]
	}
	return ""
}

func convertReader(path string, rc io.ReadCloser) (io.ReadCloser, error) {
	c := converters[agentOf(path)]
	if c == nil {
		return rc, nil
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		for _, l := range c(raw) {
			out.Write(l)
			out.WriteByte('\n')
		}
	}
	return io.NopCloser(&out), nil
}

type convertingStore struct{ SessionStore }

func (s convertingStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	metas, err := s.SessionStore.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := metas[:0]
	for _, m := range metas {
		if isListedSession(m.Path) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s convertingStore) OpenSession(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := s.SessionStore.OpenSession(ctx, path)
	if err != nil {
		return nil, err
	}
	return convertReader(path, rc)
}

func (s convertingStore) SessionLines(ctx context.Context, path string, after int64) ([][]byte, int64, error) {
	ls, ok := s.SessionStore.(sessionLineStore)
	if !ok {
		return nil, 0, errNoLineStore
	}
	lines, cursor, err := ls.SessionLines(ctx, path, after)
	if err != nil {
		return nil, 0, err
	}
	c := converters[agentOf(path)]
	if c == nil {
		return lines, cursor, nil
	}
	out := make([][]byte, 0, len(lines))
	for _, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		out = append(out, c(raw)...)
	}
	return out, cursor, nil
}

var errNoLineStore = errors.New("session store has no line access")

type convertingHeadStore struct{ convertingStore }

func (s convertingHeadStore) OpenSessionHead(ctx context.Context, path string, limit int) (io.ReadCloser, error) {
	rc, err := s.SessionStore.(sessionHeadOpener).OpenSessionHead(ctx, path, limit)
	if err != nil {
		return nil, err
	}
	return convertReader(path, rc)
}

func withConverters(s SessionStore) SessionStore {
	if _, ok := s.(sessionHeadOpener); ok {
		return convertingHeadStore{convertingStore{s}}
	}
	return convertingStore{s}
}
