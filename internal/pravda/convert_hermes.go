package pravda

import "bytes"

func convertHermes(line []byte) [][]byte {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	return [][]byte{line}
}

func init() {
	RegisterConverter("hermes", convertHermes)
}
