package crypto

import (
	"testing"
)

const (
	testKeyFile = "testdata/test-keys.pem"
)

func TestParseKeyFile(t *testing.T) {
	curves, keys, err := ParseKeyFile(testKeyFile)
	if err != nil {
		t.Fatal(err)
	}

	if len(keys) != 2 || len(curves) != 2 {
		t.Fatalf("bad ParseKeyFile: curves %d, keys %d", len(curves), len(keys))
	}
}
