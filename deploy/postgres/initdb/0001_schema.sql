-- Esquema de la plataforma de liveness.
--
-- Separación central, y de ella depende todo lo demás:
--
--   * `sessions` es el REGISTRO DE AUDITORÍA. Veredicto, motivos, versión del
--     perfil, huellas. No contiene dato biométrico, así que puede conservarse.
--     Es estrictamente append-only: ni UPDATE ni DELETE.
--
--   * `session_timelines` y `evidence_objects` son DATO BIOMÉTRICO derivado y
--     directo. Categoría especial bajo la LOPDP ecuatoriana (art. 25). TTL
--     corto, cifrado en reposo y borrado verificable.
--
-- El job de retención borra lo segundo y deja lo primero. Un rechazo del mes
-- pasado se puede seguir explicando sin conservar la cara de nadie.

CREATE SCHEMA IF NOT EXISTS liveness;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

COMMENT ON SCHEMA liveness IS
  'Resultados de sesiones de prueba de vida. La auditoria se conserva; el dato biometrico caduca.';

-- ---------------------------------------------------------------- sesiones --

CREATE TABLE IF NOT EXISTS liveness.sessions (
    session_id       text PRIMARY KEY,
    -- Identificador del sujeto, para la politica de reintentos. Es un
    -- seudonimo que provee el llamante; aqui no se guarda identidad civil.
    subject_id       text,

    outcome          text NOT NULL CHECK (outcome IN ('pass', 'reject', 'retry')),
    score            double precision NOT NULL CHECK (score >= 0 AND score <= 1),

    -- Motivos: los codigos sueltos para filtrar, y el detalle completo para
    -- explicar. Un rechazo sin la señal, el valor y el umbral no es auditable.
    reason_codes     text[] NOT NULL DEFAULT '{}',
    reasons          jsonb  NOT NULL DEFAULT '[]'::jsonb,
    -- Señales que entraron en la fusion, con su peso y su suelo.
    signals          jsonb  NOT NULL DEFAULT '[]'::jsonb,

    -- Que perfil decidio. Sin esto, un veredicto no se puede reproducir.
    profile_version  text NOT NULL,
    profile_checksum text NOT NULL,
    analyzer_version text,

    created_at       timestamptz NOT NULL,
    decided_at       timestamptz NOT NULL,
    persisted_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sessions_decided_at_idx  ON liveness.sessions (decided_at DESC);
CREATE INDEX IF NOT EXISTS sessions_outcome_idx     ON liveness.sessions (outcome, decided_at DESC);
CREATE INDEX IF NOT EXISTS sessions_subject_idx     ON liveness.sessions (subject_id, decided_at DESC);
CREATE INDEX IF NOT EXISTS sessions_reason_codes_idx ON liveness.sessions USING gin (reason_codes);

COMMENT ON TABLE liveness.sessions IS
  'Registro de auditoria. Append-only: no admite UPDATE ni DELETE. Sin dato biometrico.';

-- ------------------------------------------------------ linea de tiempo ----
--
-- Las features por frame y por ventana. Son dato derivado de biometria: no
-- son una cara, pero salen de una. TTL corto y borrado verificable.

CREATE TABLE IF NOT EXISTS liveness.session_timelines (
    session_id text PRIMARY KEY REFERENCES liveness.sessions (session_id),
    timeline   jsonb NOT NULL,
    -- Huella del contenido, para poder probar que se borro exactamente esto.
    sha256     text NOT NULL,
    stored_at  timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS session_timelines_expiry_idx ON liveness.session_timelines (expires_at);

COMMENT ON TABLE liveness.session_timelines IS
  'Features derivadas de biometria. Categoria especial LOPDP: TTL corto, borrado verificable.';

-- ------------------------------------------------------------- evidencia ---

CREATE TABLE IF NOT EXISTS liveness.evidence_objects (
    object_id   text PRIMARY KEY,
    session_id  text NOT NULL REFERENCES liveness.sessions (session_id),
    -- key_frame: frames sueltos del recorrido.
    -- reference_frame: el mejor frame, el que servira para el match 1:1
    --   posterior contra documento.
    -- clip: el video completo de la sesion.
    kind        text NOT NULL CHECK (kind IN ('key_frame', 'reference_frame', 'clip')),

    bucket      text   NOT NULL,
    object_key  text   NOT NULL,
    size_bytes  bigint NOT NULL CHECK (size_bytes >= 0),

    -- Huella del CIFRADO, no del claro. Prueba igual de bien que se borro ese
    -- objeto exacto, y no deja en la base un identificador derivable de la
    -- persona.
    sha256      text NOT NULL,
    encryption  text NOT NULL,
    key_id      text NOT NULL,

    stored_at   timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,

    UNIQUE (bucket, object_key)
);

CREATE INDEX IF NOT EXISTS evidence_session_idx ON liveness.evidence_objects (session_id);
CREATE INDEX IF NOT EXISTS evidence_expiry_idx  ON liveness.evidence_objects (expires_at);

COMMENT ON TABLE liveness.evidence_objects IS
  'Frames y clips en el almacen de objetos. Cifrados en cliente antes de subir.';

-- --------------------------------------------------------- retencion -------
--
-- El recibo de borrado. Append-only tambien: un borrado que se puede borrar no
-- prueba nada.

CREATE TABLE IF NOT EXISTS liveness.retention_events (
    event_id    bigserial PRIMARY KEY,
    session_id  text NOT NULL,
    -- Que se borro: 'evidence_object' o 'session_timeline'.
    target_kind text NOT NULL CHECK (target_kind IN ('evidence_object', 'session_timeline')),
    target_id   text NOT NULL,
    -- Huella de lo borrado, copiada del registro original.
    sha256      text NOT NULL,
    -- Por que: caducidad, peticion del titular, orden judicial...
    reason      text NOT NULL,

    deleted_at  timestamptz NOT NULL DEFAULT now(),
    -- Se comprobo que ya no esta. Un borrado no verificado no es un borrado.
    verified    boolean     NOT NULL DEFAULT false,
    verified_at timestamptz,
    detail      text
);

CREATE INDEX IF NOT EXISTS retention_session_idx ON liveness.retention_events (session_id);
CREATE INDEX IF NOT EXISTS retention_deleted_idx ON liveness.retention_events (deleted_at DESC);

COMMENT ON TABLE liveness.retention_events IS
  'Recibos de borrado. Prueba de que el dato biometrico se elimino y cuando.';

-- ------------------------------------------------ append-only de verdad ----
--
-- No es una convencion ni un comentario: la base lo impide. Un resultado que
-- se puede reescribir no sirve para auditar nada, y un recibo de borrado que
-- se puede borrar tampoco.

CREATE OR REPLACE FUNCTION liveness.deny_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'liveness: % es append-only; % no esta permitido',
        TG_TABLE_NAME, TG_OP
        USING HINT = 'Para corregir un resultado, inserta uno nuevo. Para borrar biometria, usa el job de retencion.';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS sessions_append_only ON liveness.sessions;
CREATE TRIGGER sessions_append_only
    BEFORE UPDATE OR DELETE ON liveness.sessions
    FOR EACH ROW EXECUTE FUNCTION liveness.deny_mutation();

DROP TRIGGER IF EXISTS retention_events_append_only ON liveness.retention_events;
CREATE TRIGGER retention_events_append_only
    BEFORE UPDATE OR DELETE ON liveness.retention_events
    FOR EACH ROW EXECUTE FUNCTION liveness.deny_mutation();

-- Las tablas de biometria SI admiten DELETE: es el mecanismo de retencion.
-- Lo que no admiten es UPDATE, para que nadie altere una huella y luego
-- "verifique" un borrado contra ella.

CREATE OR REPLACE FUNCTION liveness.deny_update() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'liveness: % no admite UPDATE; solo INSERT y DELETE',
        TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS timelines_no_update ON liveness.session_timelines;
CREATE TRIGGER timelines_no_update
    BEFORE UPDATE ON liveness.session_timelines
    FOR EACH ROW EXECUTE FUNCTION liveness.deny_update();

DROP TRIGGER IF EXISTS evidence_no_update ON liveness.evidence_objects;
CREATE TRIGGER evidence_no_update
    BEFORE UPDATE ON liveness.evidence_objects
    FOR EACH ROW EXECUTE FUNCTION liveness.deny_update();

-- ------------------------------------------------------------ vistas -------

-- Lo que queda vivo de una sesion, para el backoffice.
CREATE OR REPLACE VIEW liveness.session_overview AS
SELECT
    s.session_id,
    s.subject_id,
    s.outcome,
    s.score,
    s.reason_codes,
    s.profile_version,
    s.decided_at,
    (t.session_id IS NOT NULL) AS timeline_available,
    t.expires_at               AS timeline_expires_at,
    count(e.object_id)         AS evidence_objects,
    min(e.expires_at)          AS evidence_expires_at
FROM liveness.sessions s
LEFT JOIN liveness.session_timelines t ON t.session_id = s.session_id
LEFT JOIN liveness.evidence_objects  e ON e.session_id = s.session_id
GROUP BY s.session_id, t.session_id, t.expires_at;

COMMENT ON VIEW liveness.session_overview IS
  'Estado de una sesion y de su biometria: que se conserva y cuando caduca.';
