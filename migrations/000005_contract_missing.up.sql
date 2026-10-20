-- A contract whose instance has expired is still known to the registry, but
-- it is no longer on the network. missing_since records when a refresh first
-- noticed that, so the API and the browse UI can say so. It is null while
-- the contract is live.
ALTER TABLE contracts ADD COLUMN missing_since TIMESTAMPTZ;
