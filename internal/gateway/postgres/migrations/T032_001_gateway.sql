-- T-032 model gateway: project model settings with immutable revisions, the
-- immutable per-run policy binding with its monotonic counters, admitted model
-- requests (one unresolved request per run, enforced here) and usage records.
--
--   * Every table is tenant owned with forced row level security; the runtime
--     role receives column-scoped grants and no DELETE.
--   * Composite foreign keys tie settings to projects, bindings to runs and
--     requests to the binding and attempt of the same project.
--   * Counters are nonnegative, bounded to 2^53-1 and only ever increase; policy
--     columns of a binding and the identity of a request never change; request
--     states follow the entity's transitions.
--   * No table has a column for prompts, completions, tool arguments, provider
--     error text or content digests. Raw content belongs to the optional
--     recorder, never to these rows.

CREATE TABLE model_settings_revisions (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    revision           bigint NOT NULL
                       CONSTRAINT model_settings_revisions_range CHECK (revision BETWEEN 2 AND 9007199254740991),
    enabled            boolean NOT NULL,
    endpoint           text NOT NULL
                       CONSTRAINT model_settings_revisions_endpoint CHECK (endpoint ~ '^https://[a-z0-9]' AND char_length(endpoint) <= 512),
    credential_ref     text NOT NULL
                       CONSTRAINT model_settings_revisions_credential_ref CHECK (credential_ref ~ '^secret://project/[a-z][a-z0-9-]{0,62}$'),
    credential_version bigint NOT NULL
                       CONSTRAINT model_settings_revisions_credential_version CHECK (credential_version BETWEEN 1 AND 9007199254740991),
    models             text[] NOT NULL
                       CONSTRAINT model_settings_revisions_models CHECK (cardinality(models) BETWEEN 1 AND 32),
    max_requests       bigint NOT NULL CONSTRAINT model_settings_revisions_max_requests CHECK (max_requests BETWEEN 1 AND 100000),
    token_allowance    bigint NOT NULL CONSTRAINT model_settings_revisions_allowance CHECK (token_allowance BETWEEN 1 AND 1099511627776),
    max_request_bytes  bigint NOT NULL CONSTRAINT model_settings_revisions_request_bytes CHECK (max_request_bytes BETWEEN 1 AND 4194304),
    max_input_tokens   bigint NOT NULL CONSTRAINT model_settings_revisions_input CHECK (max_input_tokens BETWEEN 1 AND 4000000),
    max_output_tokens  bigint NOT NULL CONSTRAINT model_settings_revisions_output CHECK (max_output_tokens BETWEEN 1 AND 1000000),
    recording          boolean NOT NULL,
    updated_at         timestamptz NOT NULL,
    updated_by         text NOT NULL CONSTRAINT model_settings_revisions_updated_by CHECK (char_length(updated_by) BETWEEN 1 AND 256),
    PRIMARY KEY (tenant_id, project_id, revision),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);

CREATE TABLE model_settings (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    revision           bigint NOT NULL,
    enabled            boolean NOT NULL,
    endpoint           text NOT NULL,
    credential_ref     text NOT NULL,
    credential_version bigint NOT NULL,
    models             text[] NOT NULL,
    max_requests       bigint NOT NULL,
    token_allowance    bigint NOT NULL,
    max_request_bytes  bigint NOT NULL,
    max_input_tokens   bigint NOT NULL,
    max_output_tokens  bigint NOT NULL,
    recording          boolean NOT NULL,
    updated_at         timestamptz NOT NULL,
    updated_by         text NOT NULL,
    PRIMARY KEY (tenant_id, project_id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id, revision) REFERENCES model_settings_revisions (tenant_id, project_id, revision)
);

CREATE FUNCTION t032_settings_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'model_settings_revisions' THEN
        RAISE EXCEPTION 'settings revisions are immutable' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'model settings are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION 'a settings revision advances by exactly one' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER model_settings_guard BEFORE UPDATE OR DELETE ON model_settings
    FOR EACH ROW EXECUTE FUNCTION t032_settings_guard();
CREATE TRIGGER model_settings_revisions_guard BEFORE UPDATE OR DELETE ON model_settings_revisions
    FOR EACH ROW EXECUTE FUNCTION t032_settings_guard();

-- The run's policy binding: captured once at the run's first model admission.
CREATE TABLE model_run_bindings (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    settings_revision  bigint NOT NULL,
    endpoint           text NOT NULL,
    credential_ref     text NOT NULL,
    credential_version bigint NOT NULL,
    models             text[] NOT NULL,
    max_requests       bigint NOT NULL,
    token_allowance    bigint NOT NULL,
    max_request_bytes  bigint NOT NULL,
    max_input_tokens   bigint NOT NULL,
    max_output_tokens  bigint NOT NULL,
    recording          boolean NOT NULL,
    requests_admitted  bigint NOT NULL DEFAULT 0
                       CONSTRAINT model_run_bindings_requests_range CHECK (requests_admitted BETWEEN 0 AND 100000),
    tokens_charged     bigint NOT NULL DEFAULT 0
                       CONSTRAINT model_run_bindings_tokens_range CHECK (tokens_charged BETWEEN 0 AND 9007199254740991),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, run_id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES runs (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, settings_revision) REFERENCES model_settings_revisions (tenant_id, project_id, revision),
    CONSTRAINT model_run_bindings_cap CHECK (requests_admitted <= max_requests)
);

CREATE FUNCTION t032_binding_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run bindings are retained' USING ERRCODE = '23514';
    END IF;
    IF (NEW.tenant_id, NEW.project_id, NEW.run_id, NEW.settings_revision, NEW.endpoint, NEW.credential_ref, NEW.credential_version,
        NEW.models, NEW.max_requests, NEW.token_allowance, NEW.max_request_bytes, NEW.max_input_tokens, NEW.max_output_tokens,
        NEW.recording, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.run_id, OLD.settings_revision, OLD.endpoint, OLD.credential_ref, OLD.credential_version,
        OLD.models, OLD.max_requests, OLD.token_allowance, OLD.max_request_bytes, OLD.max_input_tokens, OLD.max_output_tokens,
        OLD.recording, OLD.created_at) THEN
        RAISE EXCEPTION 'a run binding is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.requests_admitted < OLD.requests_admitted OR NEW.requests_admitted > OLD.requests_admitted + 1
       OR NEW.tokens_charged < OLD.tokens_charged THEN
        RAISE EXCEPTION 'run counters only increase' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER model_run_bindings_guard BEFORE UPDATE OR DELETE ON model_run_bindings
    FOR EACH ROW EXECUTE FUNCTION t032_binding_guard();

CREATE TABLE model_requests (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    id                 kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    ordinal            bigint NOT NULL CONSTRAINT model_requests_ordinal_range CHECK (ordinal BETWEEN 1 AND 100000),
    model              text NOT NULL CONSTRAINT model_requests_model_syntax CHECK (model ~ '^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$'),
    input_bound        bigint NOT NULL CONSTRAINT model_requests_input_range CHECK (input_bound BETWEEN 0 AND 4000000),
    output_max         bigint NOT NULL CONSTRAINT model_requests_output_range CHECK (output_max BETWEEN 1 AND 1000000),
    charge_bound       bigint NOT NULL CONSTRAINT model_requests_bound_range CHECK (charge_bound BETWEEN 1 AND 5000000),
    recording          boolean NOT NULL,
    recording_outcome  text CONSTRAINT model_requests_recording_outcome CHECK (recording_outcome IN ('recorded', 'failed')),
    state              text NOT NULL
                       CONSTRAINT model_requests_state_known CHECK (state IN ('admitted', 'forwarding', 'succeeded', 'failed', 'cancelled', 'unknown', 'settled')),
    outcome_code       text CONSTRAINT model_requests_code CHECK (outcome_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    stream_started     boolean NOT NULL DEFAULT false,
    admitted_at        timestamptz NOT NULL,
    forwarding_at      timestamptz,
    finished_at        timestamptz,
    heartbeat_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, id),
    FOREIGN KEY (tenant_id, project_id, run_id) REFERENCES model_run_bindings (tenant_id, project_id, run_id),
    FOREIGN KEY (tenant_id, project_id, attempt_id) REFERENCES step_attempts (tenant_id, project_id, id),
    CONSTRAINT model_requests_ordinal_unique UNIQUE (tenant_id, project_id, run_id, ordinal),
    CONSTRAINT model_requests_bound_sum CHECK (charge_bound = input_bound + output_max),
    CONSTRAINT model_requests_shape CHECK (
        (state = 'admitted' AND forwarding_at IS NULL AND finished_at IS NULL)
        OR (state = 'forwarding' AND forwarding_at IS NOT NULL AND finished_at IS NULL)
        OR (state IN ('succeeded', 'failed', 'cancelled', 'unknown', 'settled') AND finished_at IS NOT NULL AND outcome_code IS NOT NULL))
);
-- At most one unresolved request per run, across all replicas: a request in
-- flight and a request whose remote outcome is unknown both hold the slot.
CREATE UNIQUE INDEX model_requests_one_unresolved ON model_requests (tenant_id, project_id, run_id)
    WHERE state IN ('admitted', 'forwarding', 'unknown');
CREATE INDEX model_requests_attempt_idx ON model_requests (tenant_id, project_id, attempt_id);
CREATE INDEX model_requests_abandoned_idx ON model_requests (tenant_id, heartbeat_at) WHERE state IN ('admitted', 'forwarding');

-- The attempt must belong to the request's run (the attempt table's own key
-- cannot be referenced with the run column).
CREATE FUNCTION t032_request_owner_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM step_attempts a WHERE a.tenant_id = NEW.tenant_id AND a.project_id = NEW.project_id
                         AND a.id = NEW.attempt_id AND a.run_id = NEW.run_id) THEN
            RAISE EXCEPTION 'the attempt does not belong to the run' USING ERRCODE = '23503';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'model requests are retained' USING ERRCODE = '23514';
    END IF;
    IF (NEW.tenant_id, NEW.project_id, NEW.id, NEW.run_id, NEW.attempt_id, NEW.ordinal, NEW.model, NEW.input_bound, NEW.output_max,
        NEW.charge_bound, NEW.recording, NEW.admitted_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.project_id, OLD.id, OLD.run_id, OLD.attempt_id, OLD.ordinal, OLD.model, OLD.input_bound, OLD.output_max,
        OLD.charge_bound, OLD.recording, OLD.admitted_at) THEN
        RAISE EXCEPTION 'a model request identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state AND NOT (
        (OLD.state = 'admitted' AND NEW.state IN ('forwarding', 'failed'))
        OR (OLD.state = 'forwarding' AND NEW.state IN ('succeeded', 'failed', 'cancelled', 'unknown'))
        OR (OLD.state = 'unknown' AND NEW.state = 'settled')) THEN
        RAISE EXCEPTION 'illegal model request transition % -> %', OLD.state, NEW.state USING ERRCODE = '23514';
    END IF;
    IF OLD.state IN ('succeeded', 'failed', 'cancelled', 'settled') AND
       (NEW.state, NEW.outcome_code, NEW.stream_started, NEW.finished_at) IS DISTINCT FROM (OLD.state, OLD.outcome_code, OLD.stream_started, OLD.finished_at) THEN
        RAISE EXCEPTION 'a settled model request is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.recording_outcome IS NOT NULL AND NEW.recording_outcome IS DISTINCT FROM OLD.recording_outcome THEN
        RAISE EXCEPTION 'a recording outcome is written once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER model_requests_guard BEFORE INSERT OR UPDATE OR DELETE ON model_requests
    FOR EACH ROW EXECUTE FUNCTION t032_request_owner_guard();

-- Exactly one usage record per request. It carries identifiers, the requested
-- and the provider-confirmed model, the actual destination, numeric usage and
-- the uncertainty code: never content.
CREATE TABLE model_usage (
    tenant_id          kodama_id NOT NULL,
    project_id         kodama_id NOT NULL,
    request_id         kodama_id NOT NULL,
    run_id             kodama_id NOT NULL,
    attempt_id         kodama_id NOT NULL,
    requested_model    text NOT NULL,
    actual_model       text,
    model_confirmed    boolean NOT NULL,
    destination_origin text NOT NULL CONSTRAINT model_usage_origin CHECK (char_length(destination_origin) BETWEEN 1 AND 300),
    destination_ip     text CONSTRAINT model_usage_ip CHECK (char_length(destination_ip) <= 64),
    input_tokens       bigint CONSTRAINT model_usage_input_range CHECK (input_tokens BETWEEN 0 AND 9007199254740991),
    output_tokens      bigint CONSTRAINT model_usage_output_range CHECK (output_tokens BETWEEN 0 AND 9007199254740991),
    charged_tokens     bigint NOT NULL CONSTRAINT model_usage_charged_range CHECK (charged_tokens BETWEEN 0 AND 9007199254740991),
    basis              text NOT NULL CONSTRAINT model_usage_basis CHECK (basis IN ('observed', 'estimated')),
    uncertainty        text NOT NULL CONSTRAINT model_usage_uncertainty CHECK (uncertainty ~ '^[a-z][a-z0-9_]{0,63}$'),
    outcome            text NOT NULL CONSTRAINT model_usage_outcome CHECK (outcome IN ('succeeded', 'failed', 'cancelled', 'unknown')),
    outcome_code       text NOT NULL CONSTRAINT model_usage_code CHECK (outcome_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, project_id, request_id),
    FOREIGN KEY (tenant_id, project_id, request_id) REFERENCES model_requests (tenant_id, project_id, id),
    CONSTRAINT model_usage_confirmed CHECK ((model_confirmed AND actual_model IS NOT NULL) OR (NOT model_confirmed AND actual_model IS NULL)),
    CONSTRAINT model_usage_observed CHECK (basis <> 'observed' OR (input_tokens IS NOT NULL AND output_tokens IS NOT NULL AND uncertainty = 'none'))
);
CREATE INDEX model_usage_run_idx ON model_usage (tenant_id, project_id, run_id);

CREATE FUNCTION t032_usage_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'usage records are immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER model_usage_guard BEFORE UPDATE OR DELETE ON model_usage
    FOR EACH ROW EXECUTE FUNCTION t032_usage_guard();

ALTER TABLE model_settings_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_settings_revisions FORCE ROW LEVEL SECURITY;
ALTER TABLE model_settings           ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_settings           FORCE ROW LEVEL SECURITY;
ALTER TABLE model_run_bindings       ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_run_bindings       FORCE ROW LEVEL SECURITY;
ALTER TABLE model_requests           ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_requests           FORCE ROW LEVEL SECURITY;
ALTER TABLE model_usage              ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_usage              FORCE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON model_settings_revisions TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON model_settings           TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON model_run_bindings       TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON model_requests           TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY owner_all ON model_usage              TO CURRENT_USER USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON model_settings_revisions
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON model_settings
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON model_run_bindings
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON model_requests
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY tenant_isolation ON model_usage
    USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());

GRANT SELECT, INSERT ON model_settings_revisions TO {{runtime_role}};
GRANT SELECT, INSERT ON model_settings TO {{runtime_role}};
GRANT UPDATE (revision, enabled, endpoint, credential_ref, credential_version, models, max_requests, token_allowance,
              max_request_bytes, max_input_tokens, max_output_tokens, recording, updated_at, updated_by)
    ON model_settings TO {{runtime_role}};
GRANT SELECT, INSERT ON model_run_bindings TO {{runtime_role}};
GRANT UPDATE (requests_admitted, tokens_charged, updated_at) ON model_run_bindings TO {{runtime_role}};
GRANT SELECT, INSERT ON model_requests TO {{runtime_role}};
GRANT UPDATE (state, outcome_code, stream_started, forwarding_at, finished_at, heartbeat_at, recording_outcome)
    ON model_requests TO {{runtime_role}};
GRANT SELECT, INSERT ON model_usage TO {{runtime_role}};
