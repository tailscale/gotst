CREATE TABLE IF NOT EXISTS test_history_schema (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    version integer NOT NULL
);

INSERT INTO test_history_schema (singleton, version)
VALUES (true, 1)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS test_history_key (
    scope_id uuid NOT NULL,
    key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    package text NOT NULL,
    test_name text NOT NULL,
    goos text NOT NULL,
    goarch text NOT NULL,
    tags jsonb NOT NULL,
    args jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_observed_at timestamptz NOT NULL,
    PRIMARY KEY (scope_id, key_hash)
);

CREATE TABLE IF NOT EXISTS test_history_observation (
    scope_id uuid NOT NULL,
    observation_id text NOT NULL,
    key_hash bytea NOT NULL,
    observed_at timestamptz NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('pass', 'fail', 'flaky')),
    duration_ns bigint NOT NULL CHECK (duration_ns >= 0),
    attempts integer NOT NULL CHECK (attempts > 0),
    peak_rss_bytes bigint CHECK (peak_rss_bytes IS NULL OR peak_rss_bytes >= 0),
    dependencies jsonb,
    PRIMARY KEY (scope_id, observation_id),
    FOREIGN KEY (scope_id, key_hash)
        REFERENCES test_history_key(scope_id, key_hash)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS test_history_observation_key_time
    ON test_history_observation
       (scope_id, key_hash, observed_at DESC, observation_id)
    INCLUDE (outcome, duration_ns, attempts, peak_rss_bytes);

CREATE INDEX IF NOT EXISTS test_history_key_last_observed
    ON test_history_key (scope_id, last_observed_at);

CREATE INDEX IF NOT EXISTS test_history_observation_time
    ON test_history_observation (scope_id, observed_at);
