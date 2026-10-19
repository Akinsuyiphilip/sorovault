ALTER TABLE contracts
    ADD COLUMN source_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_commit TEXT NOT NULL DEFAULT '';