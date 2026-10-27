-- What the last attempt to apply this entry said when it did not go through
-- and will be tried again: a deferral, such as a compose file with edits not
-- yet committed. Shown on the entry's row, because the log and the progress
-- panel of whoever clicked are otherwise the only places it is said, and an
-- unattended apply has neither. Cleared when the next attempt begins. NULL
-- when there is nothing to say.
ALTER TABLE queue_entries ADD COLUMN note TEXT;
