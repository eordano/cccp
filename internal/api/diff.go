package api

import (
	"fmt"
	"strings"
)

const maxDiffCells = 4_000_000

func unifiedDiff(aName, bName, a, b string) string {
	if a == b {
		return ""
	}
	al, bl := splitLines(a), splitLines(b)
	ops := diffOps(al, bl)

	var changes []int
	for i, o := range ops {
		if o.kind != ' ' {
			changes = append(changes, i)
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	const ctx = 3
	for k := 0; k < len(changes); {
		last := k
		for last+1 < len(changes) && changes[last+1]-changes[last] <= 2*ctx+1 {
			last++
		}
		start := max(changes[k]-ctx, 0)
		end := min(changes[last]+ctx+1, len(ops))
		aLen, bLen := 0, 0
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aLen++
			}
			if o.kind != '-' {
				bLen++
			}
		}
		aStart, bStart := ops[start].ai, ops[start].bi
		if aLen > 0 {
			aStart++
		}
		if bLen > 0 {
			bStart++
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		k = last + 1
	}
	return out.String()
}

type diffOp struct {
	kind   byte
	text   string
	ai, bi int
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	var ops []diffOp
	if n*m > maxDiffCells {
		for i, l := range a {
			ops = append(ops, diffOp{'-', l, i, 0})
		}
		for j, l := range b {
			ops = append(ops, diffOp{'+', l, n, j})
		}
		return ops
	}
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i], i, j})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, diffOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
