package stellar

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/soroworks/sorovault/internal/wasm"
)

// RPCFetcher reads contract code over the Stellar RPC JSON-RPC API.
type RPCFetcher struct {
	client *rpcclient.Client
}

var _ Fetcher = (*RPCFetcher)(nil)

// NewRPCFetcher returns a Fetcher backed by the RPC endpoint at url. If
// httpClient is nil the SDK's default is used. Close it when done.
func NewRPCFetcher(url string, httpClient *http.Client) *RPCFetcher {
	return &RPCFetcher{client: rpcclient.NewClient(url, httpClient)}
}

// Close releases the underlying RPC connection.
func (f *RPCFetcher) Close() error {
	return f.client.Close()
}

// Network returns the passphrase reported by the RPC endpoint. Reading it
// from the network rather than trusting configuration means a misconfigured
// RPC_URL cannot silently file mainnet contracts under "testnet".
func (f *RPCFetcher) Network(ctx context.Context) (string, error) {
	resp, err := f.client.GetNetwork(ctx)
	if err != nil {
		return "", fmt.Errorf("stellar: getNetwork: %w", err)
	}
	return resp.Passphrase, nil
}

// WasmHash returns the hex hash of the WASM the contract currently executes.
func (f *RPCFetcher) WasmHash(ctx context.Context, contractID string) (string, error) {
	hash, err := f.instanceWasmHash(ctx, contractID)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash[:]), nil
}

// FetchContract returns the contract's WASM along with the hash keying it.
func (f *RPCFetcher) FetchContract(ctx context.Context, contractID string) (*Contract, error) {
	hash, err := f.instanceWasmHash(ctx, contractID)
	if err != nil {
		return nil, err
	}

	code, err := f.codeByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	return &Contract{
		ID:       contractID,
		WasmHash: hex.EncodeToString(hash[:]),
		Wasm:     code,
	}, nil
}

// FetchWasm returns uploaded code by its SHA-256 hash.
func (f *RPCFetcher) FetchWasm(ctx context.Context, wasmHash string) ([]byte, error) {
	hashBytes, err := hex.DecodeString(wasmHash)
	if err != nil || len(hashBytes) != 32 {
		return nil, fmt.Errorf("stellar: invalid wasm hash %q", wasmHash)
	}
	var hash xdr.Hash
	copy(hash[:], hashBytes)
	return f.codeByHash(ctx, hash)
}

// instanceWasmHash reads the contract's instance entry and returns the hash
// of the WASM its executable points at.
func (f *RPCFetcher) instanceWasmHash(ctx context.Context, contractID string) (xdr.Hash, error) {
	var zero xdr.Hash

	key, err := instanceLedgerKey(contractID)
	if err != nil {
		return zero, err
	}

	entry, err := f.singleEntry(ctx, key)
	if err != nil {
		return zero, err
	}
	if entry == nil {
		return zero, notFoundError(contractID)
	}

	var data xdr.LedgerEntryData
	if err := unmarshalBase64(entry.DataXDR, &data); err != nil {
		return zero, fmt.Errorf("stellar: decoding instance entry for %s: %w", contractID, err)
	}
	if data.ContractData == nil {
		return zero, fmt.Errorf("stellar: entry for %s is not contract data", contractID)
	}

	instance, ok := data.ContractData.Val.GetInstance()
	if !ok {
		return zero, fmt.Errorf("stellar: entry for %s holds no contract instance", contractID)
	}

	wasmHash, ok := instance.Executable.GetWasmHash()
	if !ok {
		// A Stellar asset contract, or a future executable variant with no
		// uploaded module to fetch.
		return zero, fmt.Errorf("%w: %s", ErrNoWasm, contractID)
	}

	return xdr.Hash(wasmHash), nil
}

// codeByHash reads the contract code entry stored under a WASM hash.
func (f *RPCFetcher) codeByHash(ctx context.Context, hash xdr.Hash) ([]byte, error) {
	var key xdr.LedgerKey
	if err := key.SetContractCode(hash); err != nil {
		return nil, fmt.Errorf("stellar: building code key: %w", err)
	}

	entry, err := f.singleEntry(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: no code entry for wasm hash %x (it may have expired)", ErrWasmNotFound, hash)
	}

	var data xdr.LedgerEntryData
	if err := unmarshalBase64(entry.DataXDR, &data); err != nil {
		return nil, fmt.Errorf("stellar: decoding code entry %x: %w", hash, err)
	}
	if data.ContractCode == nil {
		return nil, fmt.Errorf("stellar: entry %x is not contract code", hash)
	}

	// The network caps contract code well below this, but a misbehaving or
	// hostile endpoint is not bound by that, so refuse the module before it
	// reaches the decoder.
	code := data.ContractCode.Code
	if len(code) > wasm.MaxModuleSize {
		return nil, fmt.Errorf("%w: code entry %x is %d bytes, limit is %d",
			wasm.ErrModuleTooLarge, hash, len(code), wasm.MaxModuleSize)
	}

	return code, nil
}

// singleEntry requests one ledger key and returns the entry, or nil when the
// network reports no live entry under that key.
func (f *RPCFetcher) singleEntry(ctx context.Context, key xdr.LedgerKey) (*protocol.LedgerEntryResult, error) {
	encoded, err := key.MarshalBinaryBase64()
	if err != nil {
		return nil, fmt.Errorf("stellar: encoding ledger key: %w", err)
	}

	resp, err := f.client.GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest{Keys: []string{encoded}})
	if err != nil {
		return nil, fmt.Errorf("stellar: getLedgerEntries: %w", err)
	}
	if len(resp.Entries) == 0 {
		return nil, nil
	}
	return &resp.Entries[0], nil
}

// instanceLedgerKey builds the ledger key of a contract's instance entry.
// Instance entries are always persistent.
func instanceLedgerKey(contractID string) (xdr.LedgerKey, error) {
	var key xdr.LedgerKey

	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return key, fmt.Errorf("stellar: %q is not a contract address: %w", contractID, err)
	}
	if len(raw) != 32 {
		return key, fmt.Errorf("stellar: contract address %q decoded to %d bytes, want 32", contractID, len(raw))
	}

	contractIDXDR := xdr.ContractId(*(*[32]byte)(raw))
	addr := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &contractIDXDR,
	}

	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	if err := key.SetContractData(addr, instanceKey, xdr.ContractDataDurabilityPersistent); err != nil {
		return key, fmt.Errorf("stellar: building instance key: %w", err)
	}
	return key, nil
}

// unmarshalBase64 decodes a base64-wrapped XDR value as returned by RPC.
func unmarshalBase64(encoded string, v any) error {
	if encoded == "" {
		return errors.New("empty xdr payload")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("base64: %w", err)
	}
	if err := xdr.SafeUnmarshal(raw, v); err != nil {
		return err
	}
	return nil
}

// ValidateContractID reports whether s is a well-formed contract address.
// The CLI and API use it to reject bad input before spending a round trip.
func ValidateContractID(s string) error {
	if _, err := strkey.Decode(strkey.VersionByteContract, s); err != nil {
		return fmt.Errorf("%q is not a valid contract address: %w", s, err)
	}
	return nil
}
