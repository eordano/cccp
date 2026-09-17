package pravda

import "bytes"

func convertOpencode(line []byte) [][]byte {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	return [][]byte{line}
}

func init() {
	RegisterConverter("opencode", convertOpencode)
}
