-- Many contracts share one WASM: every instance of a token template, every
-- vault a factory deployed. Finding them all is a lookup by hash, and the
-- hash is what the column already holds.
CREATE INDEX contracts_by_wasm_hash ON contracts (current_wasm_hash);
