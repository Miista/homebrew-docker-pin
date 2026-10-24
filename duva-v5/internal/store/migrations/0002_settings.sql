-- Settings, read through configy: an environment variable
-- DUVA_SETTING_<KEY> always wins, and this table is what is used otherwise.
-- Empty until something writes it, which today nothing does.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
