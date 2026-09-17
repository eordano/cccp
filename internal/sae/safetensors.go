package sae

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
)

type tensorInfo struct {
	Dtype   string `json:"dtype"`
	Shape   []int  `json:"shape"`
	Offsets [2]int `json:"data_offsets"`
}

type safetensors struct {
	header map[string]tensorInfo
	data   []byte
}

func readSafetensors(path string) (*safetensors, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("%s: short file", path)
	}
	n := binary.LittleEndian.Uint64(raw[:8])
	if n > uint64(len(raw)-8) {
		return nil, fmt.Errorf("%s: truncated header", path)
	}
	var header map[string]tensorInfo
	if err := json.Unmarshal(raw[8:8+n], &header); err != nil {
		return nil, fmt.Errorf("%s: header: %w", path, err)
	}
	delete(header, "__metadata__")
	return &safetensors{header: header, data: raw[8+n:]}, nil
}

func (s *safetensors) f32(name string) ([]float32, []int, error) {
	t, ok := s.header[name]
	if !ok || t.Dtype != "F32" {
		return nil, nil, fmt.Errorf("tensor %s missing or not F32", name)
	}
	if t.Offsets[0] < 0 || t.Offsets[1] < t.Offsets[0] || t.Offsets[1] > len(s.data) {
		return nil, nil, fmt.Errorf("tensor %s has invalid offsets", name)
	}
	b := s.data[t.Offsets[0]:t.Offsets[1]]
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, t.Shape, nil
}

func (s *safetensors) i64(name string) ([]int64, []int, error) {
	t, ok := s.header[name]
	if !ok || t.Dtype != "I64" {
		return nil, nil, fmt.Errorf("tensor %s missing or not I64", name)
	}
	if t.Offsets[0] < 0 || t.Offsets[1] < t.Offsets[0] || t.Offsets[1] > len(s.data) {
		return nil, nil, fmt.Errorf("tensor %s has invalid offsets", name)
	}
	b := s.data[t.Offsets[0]:t.Offsets[1]]
	out := make([]int64, len(b)/8)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return out, t.Shape, nil
}
