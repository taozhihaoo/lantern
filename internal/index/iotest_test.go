package index

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
)

func shaSum(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
