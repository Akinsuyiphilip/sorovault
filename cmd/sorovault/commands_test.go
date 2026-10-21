package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/store"
)

func TestPrintVersions(t *testing.T) {
	t.Parallel()

	versions := []store.SpecVersion{
		{WasmHash: "newer-hash", DecodedAt: time.Date(2026, 10, 2, 9, 15, 0, 0, time.UTC), Current: true},
		{WasmHash: "older-hash", DecodedAt: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)},
	}

	var out bytes.Buffer
	printVersions(&out, versions)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.Len(t, lines, 3, "a header and one row per version")
	assert.Contains(t, lines[0], "WASM HASH")
	assert.Contains(t, lines[0], "DECODED")
	assert.Contains(t, lines[0], "CURRENT")

	assert.Contains(t, lines[1], "newer-hash")
	assert.Contains(t, lines[1], "2026-10-02 09:15")
	assert.Contains(t, lines[1], "yes")

	assert.Contains(t, lines[2], "older-hash")
	assert.Contains(t, lines[2], "2026-09-01 08:00")
	assert.NotContains(t, lines[2], "yes", "a superseded version is not marked current")
}

// TestVersionsJSONShape keeps the command's JSON the same shape the API
// serves, so a pipeline can read either.
func TestVersionsJSONShape(t *testing.T) {
	t.Parallel()

	doc := versionsDocument{
		ContractID: "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN",
		Network:    "testnet",
		Versions:   []store.SpecVersion{{WasmHash: "abc", Current: true}},
	}

	raw, err := json.Marshal(doc)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, doc.ContractID, decoded["contract_id"])
	assert.Equal(t, "testnet", decoded["network"])

	versions, ok := decoded["versions"].([]any)
	require.True(t, ok)
	require.Len(t, versions, 1)
	assert.Equal(t, "abc", versions[0].(map[string]any)["wasm_hash"])
	assert.Equal(t, true, versions[0].(map[string]any)["current"])
}

func TestPackageDirName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "zkvote", packageDirName("@acme/zkvote"), "a scope is not a directory")
	assert.Equal(t, "zkvote-client", packageDirName("zkvote-client"))
}

func TestVersionsCommandTakesOneContract(t *testing.T) {
	t.Parallel()

	cmd := newVersionsCmd()
	assert.NotNil(t, cmd.Flags().Lookup("network"))
	assert.NotNil(t, cmd.Flags().Lookup("json"))

	require.Error(t, cmd.Args(cmd, nil), "a contract ID is required")
	require.Error(t, cmd.Args(cmd, []string{"a", "b"}), "only one contract ID is accepted")
	require.NoError(t, cmd.Args(cmd, []string{"a"}))
}
