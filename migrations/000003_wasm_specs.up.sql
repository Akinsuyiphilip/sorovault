-- Uploaded code can be catalogued before any contract instance uses it.
CREATE TABLE wasm_specs (
    network    TEXT        NOT NULL,
    wasm_hash  TEXT        NOT NULL,
    spec       JSONB       NOT NULL,
    decoded_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (network, wasm_hash)
);