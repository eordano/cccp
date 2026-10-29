package syncclass

import (
	"path"
	"strings"
)

func init() {
	byAgent["apiary"] = classifyApiary
}

func classifyApiary(rel string, mode uint32) Class {
	if mode&0o111 != 0 {
		return ClassPrivileged
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".txt":
		return ClassKnowledge
	}
	return ClassPrivileged
}
