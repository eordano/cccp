package sae

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

var ErrWeightsMismatch = errors.New("sae.safetensors does not hash to result.json's encoder_sha256")

func installed(dir string) bool {
	for _, p := range []string{filepath.Join(dir, "sae.safetensors"), scalePath(dir)} {
		f, err := os.Stat(p)
		if err != nil || !f.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func InstallWeights(root, run string, weights io.Reader) (string, error) {
	var result struct {
		Encoder string `json:"encoder_sha256"`
	}
	raw, err := os.ReadFile(filepath.Join(run, "result.json"))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("result.json: %w", err)
	}
	enc := result.Encoder
	if !safeEncoderID(enc) {
		return "", errors.New("result.json: encoder_sha256 is not a SHA-256")
	}
	dir := filepath.Join(root, enc)
	if installed(dir) {
		return enc, nil
	}
	st, err := readSafetensors(filepath.Join(run, "scored.safetensors"))
	if err != nil {
		return "", err
	}
	scale, shape, err := st.f32("scale")
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	out, err := os.Create(filepath.Join(tmp, "sae.safetensors"))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), weights)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != enc {
		return "", ErrWeightsMismatch
	}
	if err := writeScale(filepath.Join(tmp, "scale.safetensors"), scale, shape); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o750); err != nil {
		return "", err
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil && !installed(dir) {
		return "", err
	}
	return enc, nil
}

func writeScale(path string, scale []float32, shape []int) error {
	hb, err := json.Marshal(map[string]tensorInfo{"scale": {Dtype: "F32", Shape: shape, Offsets: [2]int{0, 4 * len(scale)}}})
	if err != nil {
		return err
	}
	if pad := len(hb) % 8; pad != 0 {
		hb = append(hb, bytes.Repeat([]byte(" "), 8-pad)...)
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint64(len(hb)))
	b.Write(hb)
	for _, v := range scale {
		binary.Write(&b, binary.LittleEndian, math.Float32bits(v))
	}
	return os.WriteFile(path, b.Bytes(), 0o640)
}
