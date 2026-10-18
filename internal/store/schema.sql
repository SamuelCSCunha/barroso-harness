-- Log de eventos append-only das sessões do Barroso Harness.
--
-- Três tabelas e uma obrigação: `events` é append-only. Nada aqui é editado
-- ou apagado durante a vida de uma sessão — o histórico do transcript se
-- reconstrói pela ordem de `seq`, e a reprodutibilidade do log depende disso.
--
-- `sessions` é a única linha mutável: `updated_at` (TouchSession), `title`
-- (SetTitle) e `metadata_json`. São metadados de índice, não conteúdo.
--
-- `blobs` guarda o que não cabe (nem deve) no log: saídas grandes de tool.
-- O evento referencia por {"blob": "<sha256hex>", "size": N}, então o log
-- continua curto e legível sem perder o dado.

CREATE TABLE IF NOT EXISTS sessions (
    id             TEXT PRIMARY KEY,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    metadata_json  TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS events (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL REFERENCES sessions(id),
    ts            TEXT NOT NULL,
    kind          TEXT NOT NULL,
    payload_json  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_session_seq
    ON events(session_id, seq);

CREATE TABLE IF NOT EXISTS blobs (
    hash  TEXT PRIMARY KEY,
    size  INTEGER NOT NULL,
    data  BLOB NOT NULL
);
