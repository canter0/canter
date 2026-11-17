ALTER TABLE ci_runners DROP CONSTRAINT ci_runners_check;
ALTER TABLE ci_runners ADD CONSTRAINT ci_runners_rate_contract CHECK (
    (rate_version='ci-free-hour-2026-11-v1' AND (
        (size_id='linux-x64-2' AND rate_micros_per_minute=2000) OR
        (size_id='linux-x64-4' AND rate_micros_per_minute=4000)
    )) OR
    (rate_version='ci-free-hour-2026-11-v2' AND (
        (size_id='linux-x64-2' AND rate_micros_per_minute=1000) OR
        (size_id='linux-x64-4' AND rate_micros_per_minute=2000)
    ))
);
