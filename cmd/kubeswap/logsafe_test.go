package main

import (
	"bytes"
	"log"
	"testing"
)

// TestKubeswap_Logf Verifies line breaks in logged values are escaped and ordinary names print unchanged.
func TestKubeswap_Logf(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	cases := map[string]string{
		"llm/qwen-7b":                  "deployment llm/qwen-7b\n",
		"name\n2026/11/02 forged line": "deployment name\\n2026/11/02 forged line\n",
		"a\r\nb":                       "deployment a\\r\\nb\n",
	}
	for in, want := range cases {
		buf.Reset()
		logf("deployment %s", in)
		if got := buf.String(); got != want {
			t.Errorf("logf(%q) wrote %q, want %q", in, got, want)
		}
	}
}
